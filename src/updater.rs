//! 应用自动更新：版本检测、下载校验、安装交接。
//!
//! 更新源是 GitHub Releases（`meimingqi222/devin-usage-metrics`），没有独立
//! 更新服务器。正式通道只认 `releases/latest` 里非 draft / 非 prerelease 的那一条。
//! 版本比较、资产选择、sidecar 校验与解压来自 `gpui-app-update-core`；
//! 「替换正在运行的二进制并重启」由 `gpui-app-update-installer` 的 helper 执行，
//! 本文件保留资产名、HTTP（ureq 3）、缓存路径、安装预检与文案。

#[cfg(target_os = "macos")]
use gpui_app_update_installer::enclosing_app_bundle;
use std::io::{Read, Write};
use std::path::{Path, PathBuf};
use std::time::Duration;

pub const GITHUB_OWNER: &str = "meimingqi222";
pub const GITHUB_REPO: &str = "devin-usage-metrics";
const USER_AGENT: &str = concat!("AgentUsageMetrics-Updater/", env!("CARGO_PKG_VERSION"));

/// 启动后首次检查延迟。
pub const FIRST_CHECK_DELAY_MS: u64 = 10_000;
/// 周期检查间隔（前一次检查结束后再计时）。
pub const CHECK_INTERVAL_MS: u64 = 4 * 60 * 60 * 1000;

#[cfg_attr(not(windows), allow(dead_code))]
const WINDOWS_EXE_NAME: &str = "devin-usage-metrics.exe";
#[cfg_attr(not(target_os = "macos"), allow(dead_code))]
const MACOS_APP_NAME: &str = "Agent Usage Metrics.app";

/// 更新进度。
#[derive(Clone, Debug, Default, PartialEq)]
pub struct DownloadProgress {
    pub percent: f32,
    pub transferred: u64,
    pub total: Option<u64>,
}

/// 更新状态机。UI 与调度都只认这个枚举。
#[derive(Clone, Debug, PartialEq)]
pub enum UpdateStatus {
    Idle {
        current_version: String,
    },
    Checking {
        current_version: String,
    },
    NotAvailable {
        current_version: String,
    },
    Available {
        current_version: String,
        latest_version: String,
        release_url: String,
        notes: String,
        asset_name: String,
        asset_url: String,
        checksum_url: String,
    },
    Downloading {
        current_version: String,
        latest_version: String,
        progress: DownloadProgress,
        asset_name: String,
        asset_url: String,
        checksum_url: String,
        release_url: String,
        notes: String,
    },
    Verifying {
        current_version: String,
        latest_version: String,
    },
    Downloaded {
        current_version: String,
        latest_version: String,
        /// 解压后待安装的路径：Windows 为 exe，macOS 为 `.app`。
        payload: PathBuf,
    },
    Installing {
        current_version: String,
        latest_version: String,
    },
    Error {
        current_version: String,
        operation: UpdateOperation,
        message: String,
    },
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum UpdateOperation {
    Check,
    Download,
    Install,
}

impl UpdateStatus {
    pub fn current_version(&self) -> &str {
        match self {
            UpdateStatus::Idle { current_version }
            | UpdateStatus::Checking { current_version }
            | UpdateStatus::NotAvailable { current_version }
            | UpdateStatus::Available {
                current_version, ..
            }
            | UpdateStatus::Downloading {
                current_version, ..
            }
            | UpdateStatus::Verifying {
                current_version, ..
            }
            | UpdateStatus::Downloaded {
                current_version, ..
            }
            | UpdateStatus::Installing {
                current_version, ..
            }
            | UpdateStatus::Error {
                current_version, ..
            } => current_version,
        }
    }

    pub fn latest_version(&self) -> Option<&str> {
        match self {
            UpdateStatus::Available { latest_version, .. }
            | UpdateStatus::Downloading { latest_version, .. }
            | UpdateStatus::Verifying { latest_version, .. }
            | UpdateStatus::Downloaded { latest_version, .. }
            | UpdateStatus::Installing { latest_version, .. } => Some(latest_version),
            _ => None,
        }
    }

    /// 侧栏是否应显示更新入口。
    pub fn wants_attention(&self) -> bool {
        matches!(
            self,
            UpdateStatus::Available { .. }
                | UpdateStatus::Downloading { .. }
                | UpdateStatus::Verifying { .. }
                | UpdateStatus::Downloaded { .. }
                | UpdateStatus::Installing { .. }
                | UpdateStatus::Error { .. }
        )
    }
}

pub use gpui_app_update_core::{
    is_skipped, is_version_newer, looks_like_dev_build, normalize_version, parse_checksum_sidecar,
    select_asset_by_candidates, sha256_file, GithubAsset, GithubRelease,
};

/// 当前构建应订阅的发行包目标。
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum UpdateTarget {
    WindowsX64,
    MacosAarch64,
    MacosX86_64,
}

pub fn current_target() -> UpdateTarget {
    #[cfg(all(target_os = "windows", target_arch = "x86_64"))]
    {
        UpdateTarget::WindowsX64
    }
    #[cfg(all(target_os = "macos", target_arch = "aarch64"))]
    {
        UpdateTarget::MacosAarch64
    }
    #[cfg(all(target_os = "macos", target_arch = "x86_64"))]
    {
        UpdateTarget::MacosX86_64
    }
    #[cfg(not(any(
        all(target_os = "windows", target_arch = "x86_64"),
        target_os = "macos"
    )))]
    {
        UpdateTarget::WindowsX64
    }
}

/// 本平台的候选资产名列表（优先序）。名称与 `.github/workflows/release.yml` 钉死。
pub fn candidate_asset_names(target: UpdateTarget) -> &'static [&'static str] {
    match target {
        UpdateTarget::WindowsX64 => &["devin-usage-metrics-windows-x64.zip"],
        UpdateTarget::MacosAarch64 => &["devin-usage-metrics-macos-arm64.zip"],
        UpdateTarget::MacosX86_64 => &["devin-usage-metrics-macos-x64.zip"],
    }
}

/// 校验下载包与 sidecar；消息沿用原有英文文案。
///
/// SHA-256 sidecar 与安装包来自同一发布源，只能发现损坏或截断，不是发布者认证。
pub fn verify_checksum(zip_path: &Path, sidecar: &str) -> Result<(), String> {
    gpui_app_update_core::verify_sha256_sidecar(zip_path, sidecar).map_err(|e| e.to_string())
}

/// 在 `cache_dir` 下新建独占 staging 目录并解压，返回待安装载荷路径。
///
/// 整个归档先校验（逃逸、绝对路径、重复项、符号链接、数量和体积上限）再写入；
/// 只会清理本产品缓存里旧的 staging 与旧版固定的 `extracted` 目录。
pub fn extract_update_zip(zip_path: &Path, cache_dir: &Path) -> Result<PathBuf, String> {
    let _ = std::fs::remove_dir_all(cache_dir.join("extracted"));
    let staging = gpui_app_update_core::create_staging_dir(cache_dir)
        .map_err(|e| format!("create extract dir: {e}"))?;
    gpui_app_update_core::extract_zip(
        zip_path,
        &staging,
        gpui_app_update_core::ExtractLimits::default(),
    )
    .map_err(|e| e.to_string())?;
    match payload_spec() {
        Some(spec) => gpui_app_update_core::find_payload(&staging, spec).map_err(|e| e.to_string()),
        None => Err("unsupported platform for app update".into()),
    }
}

fn payload_spec() -> Option<gpui_app_update_core::PayloadSpec<'static>> {
    #[cfg(windows)]
    {
        Some(gpui_app_update_core::PayloadSpec::WindowsExe(
            WINDOWS_EXE_NAME,
        ))
    }
    #[cfg(target_os = "macos")]
    {
        Some(gpui_app_update_core::PayloadSpec::MacApp(MACOS_APP_NAME))
    }
    #[cfg(not(any(windows, target_os = "macos")))]
    {
        None
    }
}

fn http_agent(timeout: Duration) -> ureq::Agent {
    ureq::Agent::config_builder()
        .timeout_global(Some(timeout))
        .build()
        .new_agent()
}

/// 阻塞拉取 latest release JSON。
pub fn fetch_latest_release(timeout_ms: u64) -> Result<GithubRelease, String> {
    let url = format!("https://api.github.com/repos/{GITHUB_OWNER}/{GITHUB_REPO}/releases/latest");
    let mut body = http_agent(Duration::from_millis(timeout_ms))
        .get(&url)
        .header("User-Agent", USER_AGENT)
        .header("Accept", "application/vnd.github+json")
        .call()
        .map_err(|e| format!("github api: {e}"))?
        .into_body();
    let text = body
        .read_to_string()
        .map_err(|e| format!("decode release json: {e}"))?;
    serde_json::from_str(&text).map_err(|e| format!("decode release json: {e}"))
}

/// 下载 URL 到文件；`on_progress` 为 (transferred, total)。
pub fn download_to_file(
    url: &str,
    dest: &Path,
    on_progress: &mut dyn FnMut(u64, Option<u64>),
) -> Result<(), String> {
    if let Some(parent) = dest.parent() {
        std::fs::create_dir_all(parent).map_err(|e| format!("create download dir: {e}"))?;
    }
    let resp = http_agent(Duration::from_secs(300))
        .get(url)
        .header("User-Agent", USER_AGENT)
        .call()
        .map_err(|e| format!("download: {e}"))?;
    let total = resp
        .headers()
        .get("Content-Length")
        .and_then(|v| v.to_str().ok())
        .and_then(|v| v.parse::<u64>().ok());
    // ureq 3 默认 body 上限约 10MB；发行 zip 会超过，必须放开。
    let mut body = resp.into_body();
    let configured = body.with_config().limit(512 * 1024 * 1024);
    let mut reader = configured.reader();
    let mut file =
        std::fs::File::create(dest).map_err(|e| format!("create {}: {e}", dest.display()))?;
    let mut buf = [0u8; 64 * 1024];
    let mut transferred = 0u64;
    loop {
        let n = reader
            .read(&mut buf)
            .map_err(|e| format!("read download: {e}"))?;
        if n == 0 {
            break;
        }
        file.write_all(&buf[..n])
            .map_err(|e| format!("write download: {e}"))?;
        transferred += n as u64;
        on_progress(transferred, total);
    }
    Ok(())
}

pub fn fetch_text(url: &str, timeout_ms: u64) -> Result<String, String> {
    let mut body = http_agent(Duration::from_millis(timeout_ms))
        .get(url)
        .header("User-Agent", USER_AGENT)
        .call()
        .map_err(|e| format!("fetch: {e}"))?
        .into_body();
    body.read_to_string().map_err(|e| format!("read body: {e}"))
}

/// 完整的「检查 → 有新版本则产出 Available」纯逻辑；网络由调用方注入。
///
/// sidecar 按「资产 URL + `.sha256`」拼接，检查阶段不要求它出现在资产列表里。
pub fn evaluate_release(
    current_version: &str,
    release: &GithubRelease,
    candidates: &[&str],
) -> Result<UpdateStatus, String> {
    let candidate = gpui_app_update_core::evaluate_release(
        current_version,
        release,
        candidates,
        gpui_app_update_core::ChecksumSource::UrlSuffix,
    )
    .map_err(|e| e.to_string())?;
    Ok(match candidate {
        None => UpdateStatus::NotAvailable {
            current_version: current_version.to_string(),
        },
        Some(c) => UpdateStatus::Available {
            current_version: current_version.to_string(),
            latest_version: c.latest_version,
            release_url: c.release_url,
            notes: c.notes,
            asset_name: c.asset_name,
            asset_url: c.asset_url,
            checksum_url: c.checksum_url,
        },
    })
}

/// 更新缓存目录：`<config_dir>/devin-usage-metrics/update-cache`。
pub fn update_cache_dir() -> Option<PathBuf> {
    dirs::config_dir().map(|d| d.join("devin-usage-metrics").join("update-cache"))
}

/// 用系统默认浏览器打开 http(s) 链接。
pub fn open_url(url: &str) {
    if !url.starts_with("https://") && !url.starts_with("http://") {
        return;
    }
    #[cfg(target_os = "windows")]
    let _ = std::process::Command::new("explorer.exe").arg(url).spawn();
    #[cfg(target_os = "macos")]
    let _ = std::process::Command::new("open").arg(url).spawn();
    #[cfg(all(not(target_os = "windows"), not(target_os = "macos")))]
    let _ = std::process::Command::new("xdg-open").arg(url).spawn();
}

/// 当前进程是否来自发行安装（不在 cargo target 目录里）。
pub fn is_packaged_install() -> bool {
    match std::env::current_exe() {
        Ok(exe) => {
            #[cfg(target_os = "macos")]
            {
                enclosing_app_bundle(&exe).is_some() && !looks_like_dev_build(&exe)
            }
            #[cfg(not(target_os = "macos"))]
            {
                !looks_like_dev_build(&exe)
            }
        }
        Err(_) => false,
    }
}

/// 启动时清理上次更新留下的 `.old` 备份。
pub fn cleanup_previous_update_leftovers() {
    let Ok(exe) = std::env::current_exe() else {
        return;
    };
    #[cfg(windows)]
    {
        gpui_app_update_installer::remove_exe_backup(&exe);
    }
    #[cfg(target_os = "macos")]
    {
        if let Some(bundle) = enclosing_app_bundle(&exe) {
            gpui_app_update_installer::remove_bundle_backup(&bundle);
        }
    }
    #[cfg(not(any(windows, target_os = "macos")))]
    {
        let _ = exe;
    }
}

/// 预检通过后拉起 helper，调用方随后退出进程。
///
/// helper（共享安装器）rename-first：任一时刻磁盘上至少有一份完整程序，
/// copy 失败时把 `.old` 挪回原名再启动。
pub fn apply_update_and_restart(payload: &Path) -> Result<(), String> {
    #[cfg(windows)]
    {
        apply_update_windows(payload)
    }
    #[cfg(target_os = "macos")]
    {
        apply_update_macos(payload)
    }
    #[cfg(not(any(windows, target_os = "macos")))]
    {
        let _ = payload;
        Err("unsupported platform for app update".into())
    }
}

#[cfg(any(windows, target_os = "macos"))]
fn probe_install_dir(parent: &Path) -> Result<(), String> {
    let probe = parent.join(format!(".aum-update-write-probe-{}", std::process::id()));
    match std::fs::write(&probe, b"ok") {
        Ok(()) => {
            let _ = std::fs::remove_file(&probe);
            Ok(())
        }
        Err(e) => Err(format!(
            "install directory is not writable ({}): {e}",
            parent.display()
        )),
    }
}

#[cfg(any(windows, target_os = "macos"))]
fn helper_dir() -> Result<PathBuf, String> {
    let cache = update_cache_dir().ok_or_else(|| "user data dir unavailable".to_string())?;
    std::fs::create_dir_all(&cache).map_err(|e| format!("create cache: {e}"))?;
    Ok(cache)
}

#[cfg(windows)]
fn apply_update_windows(payload: &Path) -> Result<(), String> {
    if !payload.is_file() {
        return Err(format!("update payload missing: {}", payload.display()));
    }
    let current = std::env::current_exe().map_err(|e| format!("current exe: {e}"))?;
    if current == payload {
        return Err("payload is the running executable".into());
    }
    let Some(parent) = current.parent() else {
        return Err("current exe has no parent directory".into());
    };
    probe_install_dir(parent)?;
    let cache = helper_dir()?;
    gpui_app_update_installer::spawn_windows_exe_replacement(&current, payload, &cache)
        .map_err(|e| e.to_string())
}

#[cfg(target_os = "macos")]
fn apply_update_macos(payload: &Path) -> Result<(), String> {
    if !payload.is_dir() {
        return Err(format!("update payload missing: {}", payload.display()));
    }
    let exe = std::env::current_exe().map_err(|e| format!("current exe: {e}"))?;
    let current_bundle =
        enclosing_app_bundle(&exe).ok_or_else(|| "not running from a .app bundle".to_string())?;
    let Some(parent) = current_bundle.parent() else {
        return Err("app bundle has no parent directory".into());
    };
    probe_install_dir(parent)?;
    let cache = helper_dir()?;
    gpui_app_update_installer::spawn_macos_bundle_replacement(&current_bundle, payload, &cache)
        .map_err(|e| e.to_string())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn newer_semver() {
        assert!(is_version_newer("0.0.8", "0.0.7"));
        assert!(is_version_newer("v0.1.0", "0.0.9"));
        assert!(is_version_newer("1.0.0", "0.9.9"));
        assert!(!is_version_newer("0.0.7", "0.0.7"));
        assert!(!is_version_newer("0.0.6", "0.0.7"));
        assert!(!is_version_newer("v0.0.7", "0.0.7"));
        assert!(is_version_newer("0.0.10", "0.0.9"));
    }

    #[test]
    fn missing_patch_treated_as_zero() {
        assert!(is_version_newer("1.2", "1.1.9"));
        assert!(!is_version_newer("1.2", "1.2.0"));
    }

    #[test]
    fn parse_sidecar() {
        assert_eq!(
            parse_checksum_sidecar("abc123...  file.zip\n"),
            None,
            "too short rejected"
        );
        let hex = "a".repeat(64);
        assert_eq!(
            parse_checksum_sidecar(&format!("{hex}  devin-usage-metrics-windows-x64.zip\r\n")),
            Some(hex.clone())
        );
        assert_eq!(parse_checksum_sidecar(""), None);
    }

    #[test]
    fn parse_sidecar_strips_bom() {
        let hex = "b".repeat(64);
        let with_bom = format!("\u{feff}{hex}  file.zip\n");
        assert_eq!(parse_checksum_sidecar(&with_bom), Some(hex));
    }

    fn sample_release(prerelease: bool, draft: bool) -> GithubRelease {
        GithubRelease {
            tag_name: "v0.2.0".into(),
            html_url: "https://example/rel".into(),
            name: Some("0.2.0".into()),
            body: Some("notes".into()),
            prerelease,
            draft,
            assets: vec![
                GithubAsset {
                    name: "devin-usage-metrics-macos-arm64.zip".into(),
                    browser_download_url: "https://example/mac.zip".into(),
                    size: 2,
                },
                GithubAsset {
                    name: "devin-usage-metrics-windows-x64.zip".into(),
                    browser_download_url: "https://example/win.zip".into(),
                    size: 1,
                },
            ],
        }
    }

    #[test]
    fn draft_and_prerelease_are_ignored() {
        let status = evaluate_release(
            "0.1.0",
            &sample_release(true, false),
            &["devin-usage-metrics-windows-x64.zip"],
        )
        .unwrap();
        assert!(matches!(status, UpdateStatus::NotAvailable { .. }));

        let status = evaluate_release(
            "0.1.0",
            &sample_release(false, true),
            &["devin-usage-metrics-windows-x64.zip"],
        )
        .unwrap();
        assert!(matches!(status, UpdateStatus::NotAvailable { .. }));
    }

    #[test]
    fn evaluate_picks_matching_asset() {
        let status = evaluate_release(
            "0.1.0",
            &sample_release(false, false),
            candidate_asset_names(UpdateTarget::MacosAarch64),
        )
        .unwrap();
        match status {
            UpdateStatus::Available {
                latest_version,
                asset_name,
                asset_url,
                checksum_url,
                notes,
                ..
            } => {
                assert_eq!(latest_version, "0.2.0");
                assert_eq!(asset_name, "devin-usage-metrics-macos-arm64.zip");
                assert_eq!(asset_url, "https://example/mac.zip");
                assert_eq!(checksum_url, "https://example/mac.zip.sha256");
                assert_eq!(notes, "notes");
            }
            other => panic!("expected Available, got {other:?}"),
        }
    }

    #[test]
    fn no_newer_version_is_not_available() {
        let mut release = sample_release(false, false);
        release.tag_name = "v0.1.0".into();
        let status = evaluate_release("0.1.0", &release, &["a.zip"]).unwrap();
        assert!(matches!(status, UpdateStatus::NotAvailable { .. }));
    }

    #[test]
    fn intel_mac_does_not_pick_arm64() {
        let status = evaluate_release(
            "0.1.0",
            &sample_release(false, false),
            candidate_asset_names(UpdateTarget::MacosX86_64),
        );
        assert!(status.unwrap_err().contains("no matching asset"));
    }

    #[test]
    fn dev_build_paths_are_detected() {
        assert!(looks_like_dev_build(Path::new(
            r"D:\code\devin-usage-metrics\target\debug\devin-usage-metrics.exe"
        )));
        assert!(looks_like_dev_build(Path::new(
            "/tmp/dum/target/release/devin-usage-metrics"
        )));
        assert!(!looks_like_dev_build(Path::new(
            r"C:\Users\me\AppData\Local\Programs\devin-usage-metrics.exe"
        )));
    }

    #[test]
    fn skipped_version_gate() {
        assert!(is_skipped("0.2.0", Some("v0.2.0")));
        assert!(!is_skipped("0.2.0", Some("0.1.0")));
        assert!(!is_skipped("0.2.0", None));
    }

    fn test_dir(name: &str) -> PathBuf {
        let dir = std::env::temp_dir().join(format!("dum-updater-{name}-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        dir
    }

    #[test]
    fn extract_rejects_zip_slip() {
        let dir = test_dir("zip-slip");
        let zip_path = dir.join("evil.zip");
        {
            let file = std::fs::File::create(&zip_path).unwrap();
            let mut zip = zip::ZipWriter::new(file);
            let opts = zip::write::SimpleFileOptions::default();
            zip.start_file("../escape.txt", opts).unwrap();
            zip.write_all(b"nope").unwrap();
            zip.finish().unwrap();
        }
        let cache = dir.join("cache");
        let err = extract_update_zip(&zip_path, &cache).unwrap_err();
        assert!(err.contains("escape") || err.contains("absolute"), "{err}");
        assert!(!dir.join("escape.txt").exists());
        let _ = std::fs::remove_dir_all(&dir);
    }

    fn write_release_zip(path: &Path, files: &[&str]) {
        let file = std::fs::File::create(path).unwrap();
        let mut zip = zip::ZipWriter::new(file);
        let opts = zip::write::SimpleFileOptions::default();
        for name in files {
            zip.start_file(*name, opts).unwrap();
            zip.write_all(b"binary").unwrap();
        }
        zip.finish().unwrap();
    }

    /// 发行包布局与 `.github/workflows/release.yml` 一致：Windows 根目录 exe + assets，
    /// macOS 为 `Agent Usage Metrics.app` bundle。
    #[test]
    fn extract_roundtrip_and_find_exe_or_app() {
        let dir = test_dir("extract-ok");
        let zip_path = dir.join("pack.zip");
        write_release_zip(
            &zip_path,
            &[
                WINDOWS_EXE_NAME,
                "assets/icon.png",
                "Agent Usage Metrics.app/Contents/Info.plist",
                "Agent Usage Metrics.app/Contents/MacOS/devin-usage-metrics",
            ],
        );
        let cache = dir.join("cache");
        std::fs::create_dir_all(cache.join("extracted")).unwrap();
        std::fs::write(cache.join("pack.zip.sha256"), b"keep").unwrap();
        let result = extract_update_zip(&zip_path, &cache);
        #[cfg(windows)]
        assert!(result.unwrap().ends_with(WINDOWS_EXE_NAME));
        #[cfg(target_os = "macos")]
        assert!(result.unwrap().ends_with(MACOS_APP_NAME));
        #[cfg(not(any(windows, target_os = "macos")))]
        assert_eq!(result.unwrap_err(), "unsupported platform for app update");
        assert!(
            !cache.join("extracted").exists(),
            "legacy fixed dir is retired"
        );
        assert!(
            cache.join("pack.zip.sha256").exists(),
            "other cache files stay"
        );
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn bare_binary_fallback_zip_is_not_an_installable_payload() {
        let dir = test_dir("bare-binary");
        let zip_path = dir.join("pack.zip");
        write_release_zip(&zip_path, &["devin-usage-metrics"]);
        let err = extract_update_zip(&zip_path, &dir.join("cache")).unwrap_err();
        assert!(
            err.contains("does not contain") || err.contains("unsupported platform"),
            "{err}"
        );
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn checksum_verification_keeps_messages() {
        let dir = test_dir("checksum");
        let file = dir.join("a.zip");
        std::fs::write(&file, b"abc").unwrap();
        let abc = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad";
        verify_checksum(&file, &format!("{abc}  a.zip\n")).unwrap();
        assert_eq!(
            verify_checksum(&file, "junk").unwrap_err(),
            "checksum sidecar is malformed"
        );
        let zero = "0".repeat(64);
        assert_eq!(
            verify_checksum(&file, &zero).unwrap_err(),
            format!("checksum mismatch: expected {zero}, got {abc}")
        );
        let _ = std::fs::remove_dir_all(&dir);
    }
}
