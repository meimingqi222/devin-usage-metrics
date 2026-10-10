//! Local, field-level compaction settings with a durable undo journal.
use crate::data::AgentKind;
use fs2::FileExt;
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use std::{collections::BTreeMap, fs, io::Write, path::PathBuf};
use toml_edit::DocumentMut;

type Result<T> = std::result::Result<T, String>;

#[derive(Clone)]
pub struct Target {
    pub path: PathBuf,
    pub key: String,
    model: Option<String>,
    journal: PathBuf,
}

#[derive(Clone, Debug)]
pub struct Setting {
    pub value: Option<Value>,
    pub effective: Option<Value>,
    pub locked: bool,
    pub ours: bool,
    pub has_undo: bool,
}

#[derive(Serialize, Deserialize)]
struct Undo {
    before: Option<Value>,
    written: Value,
    #[serde(default)]
    previous_written: Option<Value>,
}

impl Target {
    /// Undo remains available even if the selected range has no transcript.
    pub fn pending_local(agent: AgentKind) -> Result<Vec<String>> {
        let target = Self::local(agent, "claude-opus-4-6")?;
        let mut models = Vec::new();
        for id in target.journal()?.keys() {
            let Some((path, key)) = id.rsplit_once(':') else {
                continue;
            };
            if path != target.path.to_string_lossy() {
                continue;
            }
            if agent == AgentKind::Claude {
                if let Some(model) = key
                    .strip_prefix("modelSettings.")
                    .and_then(|s| s.strip_suffix(".autoCompactWindow"))
                {
                    models.push(model.into());
                }
            } else if key == target.key {
                models.push(String::new());
            }
        }
        Ok(models)
    }

    pub fn local(agent: AgentKind, model: &str) -> Result<Self> {
        let home = dirs::home_dir().ok_or("Home directory unavailable")?;
        let state = dirs::config_dir()
            .ok_or("Config directory unavailable")?
            .join("devin-usage-metrics/compaction-undo.json");
        let root = match agent {
            AgentKind::Claude => std::env::var_os("CLAUDE_CONFIG_DIR")
                .map(PathBuf::from)
                .unwrap_or_else(|| home.join(".claude")),
            AgentKind::Codex => std::env::var_os("CODEX_HOME")
                .map(PathBuf::from)
                .unwrap_or_else(|| home.join(".codex")),
            _ => return Err("Unsupported agent".into()),
        };
        Self::at(root, state, agent, model)
    }

    pub fn at(root: PathBuf, journal: PathBuf, agent: AgentKind, model: &str) -> Result<Self> {
        let (file, model, key) = match agent {
            AgentKind::Claude => {
                let parts: Vec<_> = model.split(['-', '.', '[', ']']).collect();
                let start = parts
                    .iter()
                    .position(|p| *p == "claude")
                    .ok_or("Not a native Claude model")?;
                let family = *parts.get(start + 1).ok_or("Missing Claude model family")?;
                let major = *parts.get(start + 2).ok_or("Missing Claude model version")?;
                if !matches!(family, "opus" | "sonnet" | "haiku" | "fable")
                    || major.is_empty()
                    || !major.bytes().all(|b| b.is_ascii_digit())
                {
                    return Err("Unsupported Claude model name".into());
                }
                let mut name = format!("claude-{family}-{major}");
                if let Some(minor) = parts.get(start + 3).filter(|p| {
                    !p.is_empty() && p.len() <= 2 && p.bytes().all(|b| b.is_ascii_digit())
                }) {
                    name.push('-');
                    name.push_str(minor);
                }
                let key = format!("modelSettings.{name}.autoCompactWindow");
                ("settings.json", Some(name), key)
            }
            AgentKind::Codex => ("config.toml", None, "model_auto_compact_token_limit".into()),
            _ => return Err("Unsupported agent".into()),
        };
        let path = root.join(file);
        let path = if fs::symlink_metadata(&path).is_ok_and(|m| m.file_type().is_symlink()) {
            fs::canonicalize(path).map_err(|e| e.to_string())?
        } else {
            path
        };
        Ok(Self {
            path,
            model,
            key,
            journal,
        })
    }

    pub fn proposed(&self, trigger: u64) -> Result<Value> {
        let value = if self.model.is_some() {
            trigger
                .checked_add(33_000)
                .ok_or("Threshold out of supported range")?
        } else {
            trigger
        };
        let min = if self.model.is_some() {
            100_000
        } else {
            50_000
        };
        if !(min..=1_000_000).contains(&value) {
            return Err("Threshold out of supported range".into());
        }
        Ok(json!(value))
    }

    fn id(&self) -> String {
        format!("{}:{}", self.path.display(), self.key)
    }

    fn text(&self) -> Result<String> {
        match fs::read_to_string(&self.path) {
            Ok(s) => Ok(s),
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(String::new()),
            Err(e) => Err(e.to_string()),
        }
    }

    fn json(&self, text: &str) -> Result<Value> {
        let value: Value = if text.trim().is_empty() {
            json!({})
        } else {
            serde_json::from_str(text).map_err(|e| e.to_string())?
        };
        if !value.is_object() {
            return Err("Settings must be a JSON object".into());
        }
        Ok(value)
    }

    fn value(&self, text: &str) -> Result<Option<Value>> {
        if let Some(model) = &self.model {
            let v = self.json(text)?;
            Ok(v.get("modelSettings")
                .and_then(|v| v.get(model))
                .and_then(|v| v.get("autoCompactWindow"))
                .cloned())
        } else {
            let doc: DocumentMut = text
                .parse()
                .map_err(|e: toml_edit::TomlError| e.to_string())?;
            match doc.get(&self.key) {
                None => Ok(None),
                Some(v) => v
                    .as_integer()
                    .map(|n| Some(json!(n)))
                    .ok_or("Codex threshold must be an integer".into()),
            }
        }
    }

    fn journal(&self) -> Result<BTreeMap<String, Undo>> {
        match fs::read(&self.journal) {
            Ok(b) => serde_json::from_slice(&b).map_err(|e| e.to_string()),
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(BTreeMap::new()),
            Err(e) => Err(e.to_string()),
        }
    }

    pub fn read(&self) -> Result<Setting> {
        let text = self.text()?;
        let value = self.value(&text)?;
        let mut effective = value.clone();
        let mut locked = false;
        if self.model.is_some() {
            let json = self.json(&text)?;
            if effective.is_none() {
                effective = json.get("autoCompactWindow").cloned();
            }
            for key in [
                "CLAUDE_CODE_AUTO_COMPACT_WINDOW",
                "CLAUDE_AUTOCOMPACT_PCT_OVERRIDE",
            ] {
                let process = std::env::var(key).unwrap_or_default();
                let configured = json.get("env").and_then(|v| v.get(key));
                if !process.is_empty() || configured.is_some_and(|v| v != "" && !v.is_null()) {
                    locked = true;
                }
            }
        }
        let journal = self.journal()?;
        let undo = journal.get(&self.id());
        let ours = undo.is_some_and(|u| {
            value.as_ref() == Some(&u.written)
                || u.previous_written
                    .as_ref()
                    .is_some_and(|previous| value.as_ref() == Some(previous))
        });
        Ok(Setting {
            value,
            effective,
            locked,
            ours,
            has_undo: undo.is_some(),
        })
    }

    fn edit(&self, text: &str, value: Option<Value>) -> Result<String> {
        if let Some(model) = &self.model {
            let mut root = self.json(text)?;
            if let Some(value) = value {
                let models = root
                    .as_object_mut()
                    .unwrap()
                    .entry("modelSettings")
                    .or_insert(json!({}));
                let models = models
                    .as_object_mut()
                    .ok_or("modelSettings is not an object")?;
                let entry = models.entry(model.clone()).or_insert(json!({}));
                entry
                    .as_object_mut()
                    .ok_or("Model settings is not an object")?
                    .insert("autoCompactWindow".into(), value);
            } else if let Some(models) =
                root.get_mut("modelSettings").and_then(Value::as_object_mut)
            {
                if let Some(entry) = models.get_mut(model).and_then(Value::as_object_mut) {
                    entry.remove("autoCompactWindow");
                    if entry.is_empty() {
                        models.remove(model);
                    }
                }
                if models.is_empty() {
                    root.as_object_mut().unwrap().remove("modelSettings");
                }
            }
            serde_json::to_string_pretty(&root)
                .map(|s| s + "\n")
                .map_err(|e| e.to_string())
        } else {
            let mut doc: DocumentMut = text
                .parse()
                .map_err(|e: toml_edit::TomlError| e.to_string())?;
            if let Some(v) = value {
                doc[&self.key] = toml_edit::value(v.as_i64().ok_or("Invalid threshold")?);
            } else {
                doc.remove(&self.key);
            }
            Ok(doc.to_string())
        }
    }

    /// Expected is the field shown in the UI. A changed field is never overwritten.
    pub fn change(&self, expected: Option<Value>, trigger: Option<u64>) -> Result<()> {
        fs::create_dir_all(self.journal.parent().unwrap()).map_err(|e| e.to_string())?;
        let lock = fs::OpenOptions::new()
            .create(true)
            .truncate(false)
            .read(true)
            .write(true)
            .open(self.journal.with_extension("lock"))
            .map_err(|e| e.to_string())?;
        lock.lock_exclusive().map_err(|e| e.to_string())?;
        let setting = self.read()?;
        if setting.value != expected {
            return Err("Configuration changed; refresh before trying again".into());
        }
        let text = self.text()?;
        if self.value(&text)? != expected {
            return Err("Configuration changed; refresh before trying again".into());
        }
        let mut journal = self.journal()?;
        let id = self.id();
        let value = if let Some(trigger) = trigger {
            if setting.locked {
                return Err("Environment override takes precedence; remove it first".into());
            }
            let written = self.proposed(trigger)?;
            if journal
                .get(&id)
                .is_some_and(|u| !setting.ours && u.before != setting.value)
            {
                return Err("Configuration was edited externally; undo record retained, refusing to overwrite".into());
            }
            let entry = journal.entry(id.clone()).or_insert(Undo {
                before: expected,
                written: written.clone(),
                previous_written: None,
            });
            entry.previous_written = if setting.ours {
                setting.value.clone()
            } else {
                None
            };
            entry.written = written.clone();
            Some(written)
        } else {
            let undo = journal.get(&id).ok_or("No saved change to undo")?;
            if !setting.ours && setting.value != undo.before {
                return Err("Configuration was edited externally; refusing to undo over it".into());
            }
            undo.before.clone()
        };
        let edited = self.edit(&text, value)?;
        // Journal first: a failed config write still leaves enough state to recover.
        atomic_write(
            &self.journal,
            &serde_json::to_vec_pretty(&journal).map_err(|e| e.to_string())?,
        )?;
        if self.text()? != text {
            return Err("Configuration changed during save; no settings overwritten".into());
        }
        atomic_write(&self.path, edited.as_bytes())?;
        if trigger.is_none() {
            journal.remove(&id);
        } else if let Some(entry) = journal.get_mut(&id) {
            // The previous value is only recoverable while a save is pending.
            // After success, a later return to it is an external edit.
            entry.previous_written = None;
        }
        atomic_write(
            &self.journal,
            &serde_json::to_vec_pretty(&journal).map_err(|e| e.to_string())?,
        )?;
        Ok(())
    }
}

fn atomic_write(path: &std::path::Path, bytes: &[u8]) -> Result<()> {
    let parent = path.parent().ok_or("Missing parent directory")?;
    fs::create_dir_all(parent).map_err(|e| e.to_string())?;
    let mut temp = tempfile::NamedTempFile::new_in(parent).map_err(|e| e.to_string())?;
    if let Ok(meta) = fs::metadata(path) {
        temp.as_file()
            .set_permissions(meta.permissions())
            .map_err(|e| e.to_string())?;
    }
    temp.write_all(bytes).map_err(|e| e.to_string())?;
    temp.as_file().sync_all().map_err(|e| e.to_string())?;
    temp.persist(path).map_err(|e| e.to_string())?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn reapply_restart_and_undo_preserve_existing_settings() {
        for agent in [AgentKind::Claude, AgentKind::Codex] {
            let dir = tempfile::tempdir().unwrap();
            let t = Target::at(
                dir.path().into(),
                dir.path().join("undo.json"),
                agent,
                "claude-opus-4-6-20260101",
            )
            .unwrap();
            let initial = if agent == AgentKind::Claude {
                json!({"permissions": {"allow": ["Read"]}, "modelSettings": {"claude-opus-4-6": {"autoCompactWindow": 500_000}}}).to_string()
            } else {
                format!(
                    "# preserve comment\n{} = {}\n[profiles.alt]\n{} = {}\n",
                    t.key, 500_000, t.key, 170_000
                )
            };
            fs::write(&t.path, initial).unwrap();
            t.change(t.read().unwrap().value, Some(220_000)).unwrap();
            assert_eq!(
                t.read().unwrap().value,
                Some(json!(if agent == AgentKind::Claude {
                    253_000
                } else {
                    220_000
                }))
            );
            t.change(t.read().unwrap().value, Some(260_000)).unwrap();
            let t = Target::at(
                dir.path().into(),
                dir.path().join("undo.json"),
                agent,
                "claude-opus-4.6",
            )
            .unwrap();
            assert!(t.read().unwrap().ours);
            // Returning to a previously applied value is still an external
            // edit, not ownership of the most recently applied setting.
            let saved = t.text().unwrap();
            let external = saved.replace(
                &t.proposed(260_000).unwrap().to_string(),
                &t.proposed(220_000).unwrap().to_string(),
            );
            fs::write(&t.path, &external).unwrap();
            assert!(!t.read().unwrap().ours);
            assert!(t.change(t.read().unwrap().value, None).is_err());
            assert_eq!(t.text().unwrap(), external);
            fs::write(&t.path, saved).unwrap();
            t.change(t.read().unwrap().value, None).unwrap();
            assert_eq!(t.read().unwrap().value, Some(json!(500_000)));
            assert!(!t.read().unwrap().ours);
            let text = t.text().unwrap();
            assert!(text.contains(if agent == AgentKind::Claude {
                "Read"
            } else {
                "# preserve comment"
            }));
            if agent == AgentKind::Codex {
                assert!(text.contains("170000"));
            }
        }
    }

    #[test]
    fn absent_fields_external_edits_and_environment_overrides() {
        let dir = tempfile::tempdir().unwrap();
        let t = Target::at(
            dir.path().into(),
            dir.path().join("undo.json"),
            AgentKind::Claude,
            "claude-sonnet-4-6",
        )
        .unwrap();
        t.change(None, Some(200_000)).unwrap();
        let before = t.read().unwrap().value;
        fs::write(&t.path, "{\"modelSettings\": {\"claude-sonnet-4-6\": {\"autoCompactWindow\": 600000}}, \"other\": true}").unwrap();
        assert!(t.change(before, None).is_err());
        assert!(t.change(Some(json!(600_000)), None).is_err());
        assert!(t.text().unwrap().contains("600000"));
        fs::write(&t.path, "{\"modelSettings\": {\"claude-sonnet-4-6\": {\"autoCompactWindow\": 233000}}, \"other\": true}").unwrap();
        t.change(Some(json!(233_000)), None).unwrap();
        let json = t.json(&t.text().unwrap()).unwrap();
        assert_eq!(json, json!({"other": true}));
        fs::write(
            &t.path,
            "{\"env\": {\"CLAUDE_CODE_AUTO_COMPACT_WINDOW\": \"300000\"}}",
        )
        .unwrap();
        assert!(t.read().unwrap().locked);
        assert!(t.change(None, Some(200_000)).is_err());
        fs::write(&t.path, "not json").unwrap();
        assert!(t.change(None, Some(200_000)).is_err());
        // Codex's absent top-level key is removed on Undo, not replaced by
        // an invented default; nested profile values and comments survive.
        let t = Target::at(
            dir.path().into(),
            dir.path().join("codex-undo.json"),
            AgentKind::Codex,
            "",
        )
        .unwrap();
        let initial = format!("# original\n[profiles.alt]\n{} = {}\n", t.key, 170_000);
        fs::write(&t.path, &initial).unwrap();
        t.change(None, Some(220_000)).unwrap();
        t.change(Some(json!(220_000)), None).unwrap();
        assert_eq!(t.read().unwrap().value, None);
        assert_eq!(t.text().unwrap(), initial);
    }
}
