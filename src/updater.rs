//! 应用自动更新：版本检测、下载校验、安装交接。
//!
//! 更新源是 GitHub Releases（`meimingqi222/devin-usage-metrics`），没有独立
//! 更新服务器。正式通道只认 `releases/latest` 里非 draft / 非 prerelease 的那一条。
//! 「替换正在运行的二进制并重启」走本文件底部的平台 helper。

use serde::Deserialize;
use sha2::{Digest, Sha256};
use std::io::{Read, Write};
use std::path::{Component, Path, PathBuf};
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

/// 轻量 semver：去掉可选 `v` 前缀，比较三段数字，缺省按 0。
pub fn is_version_newer(candidate: &str, current: &str) -> bool {
    let next = version_parts(candidate);
    let base = version_parts(current);
    let (Some(next), Some(base)) = (next, base) else {
        return normalize_version(candidate) != normalize_version(current);
    };
    for i in 0..next.len().max(base.len()) {
        let l = next.get(i).copied().unwrap_or(0);
        let r = base.get(i).copied().unwrap_or(0);
        if l > r {
            return true;
        }
        if l < r {
            return false;
        }
    }
    false
}

pub fn normalize_version(version: &str) -> String {
    version.trim().trim_start_matches(['v', 'V']).to_string()
}

fn version_parts(version: &str) -> Option<[u64; 3]> {
    let norm = normalize_version(version);
    let mut it = norm.split('.');
    let major = it.next()?.parse().ok()?;
    let minor = it.next().and_then(|s| s.parse().ok()).unwrap_or(0);
    let patch = it
        .next()
        .map(|s| s.split(|c: char| !c.is_ascii_digit()).next().unwrap_or("0"))
        .and_then(|s| s.parse().ok())
        .unwrap_or(0);
    Some([major, minor, patch])
}

/// GitHub Releases API 的 `releases/latest` 子集。
#[derive(Debug, Deserialize)]
pub struct GithubRelease {
    pub tag_name: String,
    pub html_url: String,
    pub name: Option<String>,
    pub body: Option<String>,
    pub prerelease: bool,
    pub draft: bool,
    pub assets: Vec<GithubAsset>,
}

#[derive(Debug, Deserialize, Clone)]
pub struct GithubAsset {
    pub name: String,
    pub browser_download_url: String,
    pub size: u64,
}

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

pub fn select_asset_by_candidates<'a>(
    release: &'a GithubRelease,
    candidates: &[&str],
) -> Option<&'a GithubAsset> {
    if release.draft || release.prerelease {
        return None;
    }
    for name in candidates {
        if let Some(asset) = release.assets.iter().find(|a| a.name == *name) {
            return Some(asset);
        }
    }
    None
}

/// 解析 `<hex>  <filename>` 形式的 sidecar，返回十六进制摘要（小写）。
///
/// 容忍 UTF-8 BOM：Windows PowerShell 5 的 `Out-File -Encoding utf8` 会写 BOM。
pub fn parse_checksum_sidecar(text: &str) -> Option<String> {
    let text = text.trim_start_matches('\u{feff}');
    let line = text.lines().find(|l| !l.trim().is_empty())?;
    let hex = line.split_whitespace().next()?;
    if hex.len() != 64 || !hex.chars().all(|c| c.is_ascii_hexdigit()) {
        return None;
    }
    Some(hex.to_ascii_lowercase())
}

/// 计算文件 SHA-256（小写 hex）。
pub fn sha256_file(path: &Path) -> std::io::Result<String> {
    let mut file = std::fs::File::open(path)?;
    let mut hasher = Sha256::new();
    let mut buf = [0u8; 64 * 1024];
    loop {
        let n = file.read(&mut buf)?;
        if n == 0 {
            break;
        }
        hasher.update(&buf[..n]);
    }
    Ok(hex_encode(&hasher.finalize()))
}

fn hex_encode(bytes: &[u8]) -> String {
    bytes.iter().map(|b| format!("{b:02x}")).collect()
}

/// 解压 zip 到 `dest_dir`，拒绝 zip-slip（任一条目逃出目标根）。
pub fn extract_update_zip(zip_path: &Path, dest_dir: &Path) -> Result<PathBuf, String> {
    if dest_dir.exists() {
        let _ = std::fs::remove_dir_all(dest_dir);
    }
    std::fs::create_dir_all(dest_dir).map_err(|e| format!("create extract dir: {e}"))?;

    let file = std::fs::File::open(zip_path).map_err(|e| format!("open zip: {e}"))?;
    let mut archive = zip::ZipArchive::new(file).map_err(|e| format!("read zip: {e}"))?;

    let dest_real = dest_dir
        .canonicalize()
        .unwrap_or_else(|_| dest_dir.to_path_buf());

    for i in 0..archive.len() {
        let mut entry = archive.by_index(i).map_err(|e| format!("zip entry: {e}"))?;
        let Some(rel) = entry.enclosed_name() else {
            return Err(format!("zip entry escapes destination: {:?}", entry.name()));
        };
        if rel.is_absolute()
            || rel
                .components()
                .any(|c| matches!(c, Component::Prefix(_) | Component::RootDir))
        {
            return Err(format!("zip entry has absolute path: {:?}", entry.name()));
        }
        let out = dest_dir.join(&rel);
        if let Ok(out_real) = out.canonicalize() {
            if !out_real.starts_with(&dest_real) {
                return Err(format!("zip entry escapes destination: {:?}", entry.name()));
            }
        }

        if entry.is_dir() || entry.name().ends_with('/') {
            std::fs::create_dir_all(&out).map_err(|e| format!("mkdir {}: {e}", out.display()))?;
            continue;
        }
        if let Some(parent) = out.parent() {
            std::fs::create_dir_all(parent)
                .map_err(|e| format!("mkdir {}: {e}", parent.display()))?;
        }
        let mut outfile =
            std::fs::File::create(&out).map_err(|e| format!("create {}: {e}", out.display()))?;
        std::io::copy(&mut entry, &mut outfile)
            .map_err(|e| format!("write {}: {e}", out.display()))?;
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            if let Some(mode) = entry.unix_mode() {
                let _ = std::fs::set_permissions(&out, std::fs::Permissions::from_mode(mode));
            }
        }
    }

    find_payload(dest_dir)
}

fn find_payload(root: &Path) -> Result<PathBuf, String> {
    #[cfg(windows)]
    {
        let exe = root.join(WINDOWS_EXE_NAME);
        if exe.is_file() {
            return Ok(exe);
        }
        if let Ok(rd) = std::fs::read_dir(root) {
            for ent in rd.flatten() {
                let p = ent.path().join(WINDOWS_EXE_NAME);
                if p.is_file() {
                    return Ok(p);
                }
            }
        }
        Err(format!("extracted zip does not contain {WINDOWS_EXE_NAME}"))
    }
    #[cfg(target_os = "macos")]
    {
        let app = root.join(MACOS_APP_NAME);
        if app.is_dir() {
            return Ok(app);
        }
        if let Ok(rd) = std::fs::read_dir(root) {
            for ent in rd.flatten() {
                let p = ent.path();
                if p.is_dir()
                    && p.extension().and_then(|e| e.to_str()) == Some("app")
                    && p.file_name().and_then(|n| n.to_str()) == Some(MACOS_APP_NAME)
                {
                    return Ok(p);
                }
            }
        }
        Err(format!("extracted zip does not contain {MACOS_APP_NAME}"))
    }
    #[cfg(not(any(windows, target_os = "macos")))]
    {
        let _ = root;
        Err("unsupported platform for app update".into())
    }
}

/// 是否运行在 cargo 构建目录里（开发态，不检查更新）。
pub fn looks_like_dev_build(exe: &Path) -> bool {
    let s = exe.to_string_lossy().to_ascii_lowercase();
    s.contains("target\\debug")
        || s.contains("target/debug")
        || s.contains("target\\release")
        || s.contains("target/release")
}

/// 用户跳过了这个版本则不提示。
pub fn is_skipped(latest: &str, skipped: Option<&str>) -> bool {
    skipped.is_some_and(|s| normalize_version(s) == normalize_version(latest))
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
pub fn evaluate_release(
    current_version: &str,
    release: &GithubRelease,
    candidates: &[&str],
) -> Result<UpdateStatus, String> {
    if release.draft || release.prerelease {
        return Ok(UpdateStatus::NotAvailable {
            current_version: current_version.to_string(),
        });
    }
    let latest = normalize_version(&release.tag_name);
    if !is_version_newer(&latest, current_version) {
        return Ok(UpdateStatus::NotAvailable {
            current_version: current_version.to_string(),
        });
    }
    let asset = select_asset_by_candidates(release, candidates)
        .ok_or_else(|| format!("no matching asset for this platform among {candidates:?}"))?;
    Ok(UpdateStatus::Available {
        current_version: current_version.to_string(),
        latest_version: latest,
        release_url: release.html_url.clone(),
        notes: release.body.clone().unwrap_or_default(),
        asset_name: asset.name.clone(),
        asset_url: asset.browser_download_url.clone(),
        checksum_url: format!("{}.sha256", asset.browser_download_url),
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

#[cfg(target_os = "macos")]
fn enclosing_app_bundle(exe: &Path) -> Option<PathBuf> {
    for ancestor in exe.ancestors() {
        if ancestor.extension().and_then(|e| e.to_str()) == Some("app") {
            return Some(ancestor.to_path_buf());
        }
    }
    None
}

/// 启动时清理上次更新留下的 `.old` 备份。
pub fn cleanup_previous_update_leftovers() {
    let Ok(exe) = std::env::current_exe() else {
        return;
    };
    #[cfg(windows)]
    {
        let mut name = exe.as_os_str().to_owned();
        name.push(".old");
        let old = PathBuf::from(name);
        if old.exists() {
            let _ = std::fs::remove_file(&old);
        }
    }
    #[cfg(target_os = "macos")]
    {
        let Some(bundle) = enclosing_app_bundle(&exe) else {
            return;
        };
        let old = bundle.with_extension("app.old");
        if old.exists() {
            let _ = std::fs::remove_dir_all(&old);
        }
    }
    #[cfg(not(any(windows, target_os = "macos")))]
    {
        let _ = exe;
    }
}

/// 写 helper 脚本 → spawn → 调用方随后退出进程。
///
/// rename-first：任一时刻磁盘上至少有一份完整二进制。copy 失败时 helper
/// 必须把 `.old` 挪回原名。
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
    let probe = parent.join(format!(".aum-update-write-probe-{}", std::process::id()));
    match std::fs::write(&probe, b"ok") {
        Ok(()) => {
            let _ = std::fs::remove_file(&probe);
        }
        Err(e) => {
            return Err(format!(
                "install directory is not writable ({}): {e}",
                parent.display()
            ));
        }
    }

    let cache = update_cache_dir().ok_or_else(|| "user data dir unavailable".to_string())?;
    std::fs::create_dir_all(&cache).map_err(|e| format!("create cache: {e}"))?;
    let script_path = cache.join("apply-update.ps1");
    let log_path = cache.join("apply-update.log");

    let q = |p: &Path| -> String {
        let s = p.to_string_lossy().replace('\'', "''");
        format!("'{s}'")
    };
    let parent_pid = std::process::id();
    let script = format!(
        r#"$ErrorActionPreference = 'Stop'
$exe = {exe}
$new = {new}
$old = "$exe.old"
$log = {log}
function Log($m) {{ Add-Content -Path $log -Value ("$(Get-Date -Format o) " + $m) }}
try {{
  Log "waiting for pid {pid}"
  Wait-Process -Id {pid} -ErrorAction SilentlyContinue
  Start-Sleep -Milliseconds 400
  if (Test-Path -LiteralPath $old) {{ Remove-Item -LiteralPath $old -Force -ErrorAction SilentlyContinue }}
  Move-Item -LiteralPath $exe -Destination $old -Force
  try {{
    Copy-Item -LiteralPath $new -Destination $exe -Force
    Log "copied new binary"
    Start-Process -FilePath $exe
    Log "relaunched"
  }} catch {{
    Log ("COPY FAILED, restoring: " + $_.Exception.Message)
    if (Test-Path -LiteralPath $exe) {{ Remove-Item -LiteralPath $exe -Force -ErrorAction SilentlyContinue }}
    if (Test-Path -LiteralPath $old) {{
      Move-Item -LiteralPath $old -Destination $exe -Force
      Start-Process -FilePath $exe
      Log "restored previous binary and relaunched"
    }} else {{
      Log "CRITICAL: no .old to restore"
    }}
    exit 1
  }}
}} catch {{
  Log ("FAILED: " + $_.Exception.Message)
  exit 1
}}
"#,
        exe = q(&current),
        new = q(payload),
        log = q(&log_path),
        pid = parent_pid,
    );
    let mut f = std::fs::File::create(&script_path).map_err(|e| format!("write helper: {e}"))?;
    f.write_all(script.as_bytes())
        .map_err(|e| format!("write helper: {e}"))?;
    drop(f);

    let mut cmd = std::process::Command::new("powershell");
    cmd.arg("-NoProfile")
        .arg("-ExecutionPolicy")
        .arg("Bypass")
        .arg("-File")
        .arg(&script_path)
        .stdin(std::process::Stdio::null())
        .stdout(std::process::Stdio::null())
        .stderr(std::process::Stdio::null());
    {
        use std::os::windows::process::CommandExt;
        const CREATE_NO_WINDOW: u32 = 0x0800_0000;
        const CREATE_NEW_PROCESS_GROUP: u32 = 0x0000_0200;
        const CREATE_BREAKAWAY_FROM_JOB: u32 = 0x0100_0000;
        cmd.creation_flags(CREATE_NO_WINDOW | CREATE_NEW_PROCESS_GROUP | CREATE_BREAKAWAY_FROM_JOB);
        if cmd.spawn().is_ok() {
            return Ok(());
        }
        cmd.creation_flags(CREATE_NO_WINDOW | CREATE_NEW_PROCESS_GROUP);
        cmd.spawn()
            .map_err(|e| format!("spawn update helper: {e}"))?;
    }
    Ok(())
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
    let probe = parent.join(format!(".aum-update-write-probe-{}", std::process::id()));
    match std::fs::write(&probe, b"ok") {
        Ok(()) => {
            let _ = std::fs::remove_file(&probe);
        }
        Err(e) => {
            return Err(format!(
                "install directory is not writable ({}): {e}",
                parent.display()
            ));
        }
    }

    let cache = update_cache_dir().ok_or_else(|| "user data dir unavailable".to_string())?;
    std::fs::create_dir_all(&cache).map_err(|e| format!("create cache: {e}"))?;
    let script_path = cache.join("apply-update.sh");
    let log_path = cache.join("apply-update.log");

    let shell_quote = |p: &Path| -> String {
        let s = p.to_string_lossy().replace('\'', "'\\''");
        format!("'{s}'")
    };
    let parent_pid = std::process::id();
    let script = format!(
        r#"#!/bin/bash
set -euo pipefail
trap '' HUP
CUR={cur}
NEW={new}
OLD="${{CUR}}.old"
LOG={log}
log() {{ echo "$(date -Iseconds) $*" >> "$LOG"; }}
log "waiting for pid {pid}"
while kill -0 {pid} 2>/dev/null; do sleep 0.2; done
sleep 0.3
rm -rf "$OLD"
mv "$CUR" "$OLD"
if cp -R "$NEW" "$CUR"; then
  log "replaced app bundle"
  open "$CUR"
  log "relaunched"
else
  log "COPY FAILED, restoring previous bundle"
  rm -rf "$CUR"
  mv "$OLD" "$CUR"
  open "$CUR"
  log "restored and relaunched previous bundle"
  exit 1
fi
"#,
        cur = shell_quote(&current_bundle),
        new = shell_quote(payload),
        log = shell_quote(&log_path),
        pid = parent_pid,
    );
    let mut f = std::fs::File::create(&script_path).map_err(|e| format!("write helper: {e}"))?;
    f.write_all(script.as_bytes())
        .map_err(|e| format!("write helper: {e}"))?;
    drop(f);
    {
        use std::os::unix::fs::PermissionsExt;
        let _ = std::fs::set_permissions(&script_path, std::fs::Permissions::from_mode(0o755));
    }

    let mut cmd = std::process::Command::new("/bin/bash");
    cmd.arg(&script_path)
        .stdin(std::process::Stdio::null())
        .stdout(std::process::Stdio::null())
        .stderr(std::process::Stdio::null());
    {
        use std::os::unix::process::CommandExt;
        // SAFETY: 子进程里只 setsid，让 helper 离开父进程会话，避免 quit 时
        // SIGHUP 把替换脚本带走。
        unsafe {
            cmd.pre_exec(|| {
                if libc::setsid() == -1 {
                    return Err(std::io::Error::last_os_error());
                }
                Ok(())
            });
        }
    }
    cmd.spawn()
        .map_err(|e| format!("spawn update helper: {e}"))?;
    Ok(())
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
        let dest = dir.join("out");
        let err = extract_update_zip(&zip_path, &dest).unwrap_err();
        assert!(err.contains("escape") || err.contains("absolute"), "{err}");
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn extract_roundtrip_and_find_exe_or_app() {
        let dir = test_dir("extract-ok");
        let zip_path = dir.join("pack.zip");
        {
            let file = std::fs::File::create(&zip_path).unwrap();
            let mut zip = zip::ZipWriter::new(file);
            let opts = zip::write::SimpleFileOptions::default();
            #[cfg(windows)]
            let payload_name = WINDOWS_EXE_NAME;
            #[cfg(target_os = "macos")]
            let payload_name = "Agent Usage Metrics.app/Contents/MacOS/devin-usage-metrics";
            #[cfg(not(any(windows, target_os = "macos")))]
            let payload_name = WINDOWS_EXE_NAME;
            zip.start_file(payload_name, opts).unwrap();
            zip.write_all(b"binary").unwrap();
            zip.finish().unwrap();
        }
        let dest = dir.join("extracted");
        let payload = extract_update_zip(&zip_path, &dest).unwrap();
        assert!(payload.exists(), "payload at {}", payload.display());
        #[cfg(windows)]
        assert!(payload.ends_with(WINDOWS_EXE_NAME));
        #[cfg(target_os = "macos")]
        assert!(payload.ends_with(MACOS_APP_NAME));
        let _ = std::fs::remove_dir_all(&dir);
    }
}
