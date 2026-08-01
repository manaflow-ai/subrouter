use std::fs::{self, File, OpenOptions};
use std::io::{self, Write};
use std::path::{Path, PathBuf};
use std::sync::Mutex;
use std::time::SystemTime;

use chrono::{DateTime, Utc};
use fs2::FileExt;
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use subtle::ConstantTimeEq;
use thiserror::Error;

pub const KEY_PREFIX: &str = "srt_";
const KEY_RANDOM_BYTES: usize = 16;
const KEY_DISPLAY_PREFIX_LEN: usize = KEY_PREFIX.len() + 8;

#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct Key {
    pub hash: String,
    pub prefix: String,
    pub created_at: DateTime<Utc>,
}

#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct Tenant {
    pub id: String,
    pub name: String,
    pub created_at: DateTime<Utc>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub keys: Vec<Key>,
}

#[derive(Clone, Debug, Default, Deserialize, Serialize)]
struct RegistryFile {
    #[serde(default)]
    tenants: Vec<Tenant>,
}

#[derive(Debug, Error)]
pub enum TenantError {
    #[error("tenant name is required")]
    NameRequired,
    #[error("key prefix is required")]
    KeyRequired,
    #[error("tenant {0:?} already exists")]
    AlreadyExists(String),
    #[error("tenant {0:?} not found")]
    NotFound(String),
    #[error("no key matching {key:?} on tenant {tenant:?}")]
    KeyNotFound { key: String, tenant: String },
    #[error("parse {path}: {source}")]
    Parse {
        path: PathBuf,
        source: serde_json::Error,
    },
    #[error(transparent)]
    Io(#[from] io::Error),
}

#[derive(Clone, Debug, Default)]
struct Cache {
    file: RegistryFile,
    fingerprint: Option<(SystemTime, u64)>,
}

#[derive(Debug)]
pub struct Registry {
    state_dir: PathBuf,
    cache: Mutex<Cache>,
}

impl Registry {
    #[must_use]
    pub fn new(state_dir: impl Into<PathBuf>) -> Self {
        Self {
            state_dir: state_dir.into(),
            cache: Mutex::new(Cache::default()),
        }
    }

    #[must_use]
    pub fn path(&self) -> PathBuf {
        self.state_dir.join("tenants.json")
    }

    #[must_use]
    pub fn tenants_dir(&self) -> PathBuf {
        self.state_dir.join("tenants")
    }

    #[must_use]
    pub fn dir(&self, id: &str) -> PathBuf {
        self.tenants_dir().join(id)
    }

    pub fn list(&self) -> Result<Vec<Tenant>, TenantError> {
        let mut cache = self
            .cache
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        Ok(self.load(&mut cache, false)?.tenants)
    }

    #[must_use]
    pub fn has_tenants(&self) -> bool {
        self.list().is_ok_and(|tenants| !tenants.is_empty())
    }

    pub fn create(&self, name: &str) -> Result<(Tenant, String), TenantError> {
        let name = name.trim();
        if name.is_empty() {
            return Err(TenantError::NameRequired);
        }
        let mut cache = self
            .cache
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        let _lock = self.lock()?;
        let mut file = self.load(&mut cache, true)?;
        if file
            .tenants
            .iter()
            .any(|tenant| tenant.name.eq_ignore_ascii_case(name))
        {
            return Err(TenantError::AlreadyExists(name.into()));
        }
        let id = hex::encode(rand::random::<[u8; 6]>());
        let (plaintext, key) = new_key();
        let tenant = Tenant {
            id: id.clone(),
            name: name.into(),
            created_at: Utc::now(),
            keys: vec![key],
        };
        fs::create_dir_all(self.dir(&id).join("codex/accounts"))?;
        file.tenants.push(tenant.clone());
        self.save(&mut cache, &file)?;
        Ok((tenant, plaintext))
    }

    pub fn create_key(&self, tenant_id: &str) -> Result<(Tenant, String), TenantError> {
        let mut cache = self
            .cache
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        let _lock = self.lock()?;
        let mut file = self.load(&mut cache, true)?;
        let tenant = file
            .tenants
            .iter_mut()
            .find(|tenant| tenant.id == tenant_id)
            .ok_or_else(|| TenantError::NotFound(tenant_id.into()))?;
        let (plaintext, key) = new_key();
        tenant.keys.push(key);
        let result = tenant.clone();
        self.save(&mut cache, &file)?;
        Ok((result, plaintext))
    }

    pub fn revoke_key(&self, tenant_id: &str, key_ref: &str) -> Result<usize, TenantError> {
        let key_ref = key_ref.trim();
        if key_ref.is_empty() {
            return Err(TenantError::KeyRequired);
        }
        let mut cache = self
            .cache
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        let _lock = self.lock()?;
        let mut file = self.load(&mut cache, true)?;
        let tenant = file
            .tenants
            .iter_mut()
            .find(|tenant| tenant.id == tenant_id)
            .ok_or_else(|| TenantError::NotFound(tenant_id.into()))?;
        let full_hash = valid_key_format(key_ref).then(|| hash_key(key_ref));
        let before = tenant.keys.len();
        tenant.keys.retain(|key| {
            key.prefix != key_ref
                && !full_hash
                    .as_ref()
                    .is_some_and(|hash| constant_time_eq(hash.as_bytes(), key.hash.as_bytes()))
        });
        let revoked = before - tenant.keys.len();
        if revoked == 0 {
            return Err(TenantError::KeyNotFound {
                key: display_key_ref(key_ref),
                tenant: tenant_id.into(),
            });
        }
        self.save(&mut cache, &file)?;
        Ok(revoked)
    }

    pub fn resolve(&self, plaintext: &str) -> Result<Option<Tenant>, TenantError> {
        if !valid_key_format(plaintext) {
            return Ok(None);
        }
        let hash = hash_key(plaintext);
        let mut cache = self
            .cache
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        let file = self.load(&mut cache, false)?;
        Ok(file.tenants.into_iter().find(|tenant| {
            tenant
                .keys
                .iter()
                .any(|key| constant_time_eq(hash.as_bytes(), key.hash.as_bytes()))
        }))
    }

    pub fn find(&self, reference: &str) -> Result<Option<Tenant>, TenantError> {
        let reference = reference.trim();
        Ok(self
            .list()?
            .into_iter()
            .find(|tenant| tenant.id == reference || tenant.name.eq_ignore_ascii_case(reference)))
    }

    fn load(&self, cache: &mut Cache, fresh: bool) -> Result<RegistryFile, TenantError> {
        let path = self.path();
        let metadata = match fs::metadata(&path) {
            Ok(metadata) => metadata,
            Err(error) if error.kind() == io::ErrorKind::NotFound => {
                *cache = Cache::default();
                return Ok(RegistryFile::default());
            }
            Err(error) => return Err(error.into()),
        };
        let fingerprint = (metadata.modified()?, metadata.len());
        if !fresh && cache.fingerprint == Some(fingerprint) {
            return Ok(cache.file.clone());
        }
        let body = fs::read(&path)?;
        let file: RegistryFile =
            serde_json::from_slice(&body).map_err(|source| TenantError::Parse { path, source })?;
        cache.file = file.clone();
        cache.fingerprint = Some(fingerprint);
        Ok(file)
    }

    fn save(&self, cache: &mut Cache, file: &RegistryFile) -> Result<(), TenantError> {
        fs::create_dir_all(&self.state_dir)?;
        let mut body = serde_json::to_vec_pretty(file).expect("registry types serialize");
        body.push(b'\n');
        let path = self.path();
        let mut temp = tempfile::NamedTempFile::new_in(&self.state_dir)?;
        temp.write_all(&body)?;
        temp.as_file().sync_all()?;
        set_private_permissions(temp.path())?;
        temp.persist(&path).map_err(|error| error.error)?;
        let metadata = fs::metadata(path)?;
        cache.file = file.clone();
        cache.fingerprint = Some((metadata.modified()?, metadata.len()));
        Ok(())
    }

    fn lock(&self) -> io::Result<RegistryLock> {
        fs::create_dir_all(&self.state_dir)?;
        let file = OpenOptions::new()
            .create(true)
            .truncate(false)
            .read(true)
            .write(true)
            .open(self.path().with_extension("json.lock"))?;
        file.lock_exclusive()?;
        Ok(RegistryLock(file))
    }
}

struct RegistryLock(File);

impl Drop for RegistryLock {
    fn drop(&mut self) {
        let _ = self.0.unlock();
    }
}

#[must_use]
pub fn valid_key_format(value: &str) -> bool {
    value.strip_prefix(KEY_PREFIX).is_some_and(|rest| {
        rest.len() == KEY_RANDOM_BYTES * 2
            && rest
                .bytes()
                .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
    })
}

#[must_use]
pub fn hash_key(key: &str) -> String {
    hex::encode(Sha256::digest(key.as_bytes()))
}

#[must_use]
pub fn display_key_ref(value: &str) -> String {
    let value = value.trim();
    if value.chars().count() <= KEY_DISPLAY_PREFIX_LEN {
        value.into()
    } else {
        format!(
            "{}…",
            value
                .chars()
                .take(KEY_DISPLAY_PREFIX_LEN)
                .collect::<String>()
        )
    }
}

fn new_key() -> (String, Key) {
    let plaintext = format!(
        "{KEY_PREFIX}{}",
        hex::encode(rand::random::<[u8; KEY_RANDOM_BYTES]>())
    );
    let key = Key {
        hash: hash_key(&plaintext),
        prefix: plaintext[..KEY_DISPLAY_PREFIX_LEN].into(),
        created_at: Utc::now(),
    };
    (plaintext, key)
}

fn constant_time_eq(left: &[u8], right: &[u8]) -> bool {
    left.len() == right.len() && bool::from(left.ct_eq(right))
}

#[cfg(unix)]
fn set_private_permissions(path: &Path) -> io::Result<()> {
    use std::os::unix::fs::PermissionsExt;
    fs::set_permissions(path, fs::Permissions::from_mode(0o600))
}

#[cfg(not(unix))]
fn set_private_permissions(_path: &Path) -> io::Result<()> {
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn creates_resolves_and_revokes_without_persisting_plaintext() {
        let temp = tempfile::tempdir().unwrap();
        let registry = Registry::new(temp.path());
        let (created, key) = registry.create("acme").unwrap();
        assert!(valid_key_format(&key));
        assert!(!fs::read_to_string(registry.path()).unwrap().contains(&key));
        assert!(registry.dir(&created.id).join("codex/accounts").is_dir());
        assert_eq!(registry.resolve(&key).unwrap().unwrap().id, created.id);
        assert!(
            registry
                .resolve("srt_00000000000000000000000000000000")
                .unwrap()
                .is_none()
        );
        assert_eq!(
            registry
                .revoke_key(&created.id, &created.keys[0].prefix)
                .unwrap(),
            1
        );
        assert!(registry.resolve(&key).unwrap().is_none());
    }

    #[test]
    fn duplicate_names_and_loose_key_prefixes_are_rejected() {
        let temp = tempfile::tempdir().unwrap();
        let registry = Registry::new(temp.path());
        let (created, _) = registry.create("acme").unwrap();
        assert!(matches!(
            registry.create("ACME"),
            Err(TenantError::AlreadyExists(_))
        ));
        assert!(matches!(
            registry.revoke_key(&created.id, "srt_"),
            Err(TenantError::KeyNotFound { .. })
        ));
    }

    #[test]
    fn validates_exact_lowercase_key_shape() {
        assert!(valid_key_format("srt_0123456789abcdef0123456789abcdef"));
        for bad in [
            "",
            "srt_",
            "srt_0123456789ABCDEF0123456789ABCDEF",
            "srt_0123456789abcdef0123456789abcde",
            "srt_0123456789abcdef0123456789abcdef0",
        ] {
            assert!(!valid_key_format(bad), "accepted {bad}");
        }
    }

    #[test]
    fn displays_only_a_key_prefix() {
        let key = "srt_0123456789abcdef0123456789abcdef";
        assert_eq!(display_key_ref(key), "srt_01234567…");
        assert!(!display_key_ref(key).contains("89abcdef"));
        assert_eq!(display_key_ref("srt_short"), "srt_short");
    }
}
