use std::collections::BTreeMap;
use std::fs;
use std::io;
use std::path::{Path, PathBuf};

use chrono::{SecondsFormat, Utc};
use serde::{Deserialize, Serialize};

use crate::storepath;

#[derive(Clone, Debug)]
pub struct Store {
    pub dir: PathBuf,
}

impl Default for Store {
    fn default() -> Self {
        Self {
            dir: storepath::codex_dir(),
        }
    }
}

#[derive(Clone, Debug, Default, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct Profile {
    pub name: String,
    pub created_at: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub last_used: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub dir: String,
}

#[derive(Default, Deserialize, Serialize)]
struct ProfilesFile {
    #[serde(default, skip_serializing_if = "String::is_empty")]
    active: String,
    #[serde(default)]
    profiles: BTreeMap<String, Profile>,
}

impl Store {
    #[must_use]
    pub fn new(dir: impl Into<PathBuf>) -> Self {
        Self { dir: dir.into() }
    }

    #[must_use]
    pub fn profiles_path(&self) -> PathBuf {
        self.dir.join("gemini.json")
    }

    #[must_use]
    pub fn list_profiles(&self) -> Vec<Profile> {
        self.read_profiles().profiles.into_values().collect()
    }

    #[must_use]
    pub fn active_profile(&self) -> String {
        self.read_profiles().active
    }

    pub fn set_active_profile(&self, name: &str) -> io::Result<()> {
        let mut data = self.read_profiles();
        let profile = data.profiles.entry(name.into()).or_insert_with(|| Profile {
            name: name.into(),
            ..Profile::default()
        });
        profile.last_used = Utc::now().to_rfc3339_opts(SecondsFormat::Secs, true);
        data.active = name.into();
        self.write_profiles(&data)
    }

    fn read_profiles(&self) -> ProfilesFile {
        fs::read(self.profiles_path())
            .ok()
            .and_then(|body| serde_json::from_slice(&body).ok())
            .unwrap_or_default()
    }

    fn write_profiles(&self, data: &ProfilesFile) -> io::Result<()> {
        fs::create_dir_all(&self.dir)?;
        let mut body = serde_json::to_vec_pretty(data).map_err(io::Error::other)?;
        body.push(b'\n');
        write_private(&self.profiles_path(), &body)
    }
}

fn write_private(path: &Path, body: &[u8]) -> io::Result<()> {
    fs::write(path, body)?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        fs::set_permissions(path, fs::Permissions::from_mode(0o600))?;
    }
    Ok(())
}
