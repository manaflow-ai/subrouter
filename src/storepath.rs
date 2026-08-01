use std::env;
use std::fs::{self, File, OpenOptions};
use std::io::{self, Read, Write};
use std::path::{Path, PathBuf};

const STATE_DIR_ENV: &str = "SUBROUTER_STATE_DIR";

#[must_use]
pub fn state_dir() -> PathBuf {
    env::var_os(STATE_DIR_ENV)
        .filter(|value| !value.to_string_lossy().trim().is_empty())
        .map(PathBuf::from)
        .unwrap_or_else(|| {
            home_dir().map_or_else(
                || PathBuf::from(".subrouter"),
                |home| home.join(".subrouter"),
            )
        })
}

fn home_dir() -> Option<PathBuf> {
    env::var_os(if cfg!(windows) { "USERPROFILE" } else { "HOME" }).map(PathBuf::from)
}

#[must_use]
pub fn legacy_codex_dir() -> PathBuf {
    home_dir()
        .unwrap_or_else(|| PathBuf::from("."))
        .join(".codex-accounts")
}

#[must_use]
pub fn codex_dir() -> PathBuf {
    let target = state_dir().join("codex");
    let _ = migrate_codex_dir(&target, &legacy_codex_dir());
    target
}

pub fn migrate_legacy_codex_dir(target: &Path) -> io::Result<()> {
    migrate_codex_dir(target, &legacy_codex_dir())
}

pub fn migrate_codex_dir(target: &Path, legacy: &Path) -> io::Result<()> {
    if target == legacy || !legacy.is_dir() || dir_non_empty(target)? {
        return Ok(());
    }
    if target.exists() {
        return copy_dir_contents(legacy, target);
    }

    let parent = target.parent().unwrap_or_else(|| Path::new("."));
    fs::create_dir_all(parent)?;
    let temp = tempfile::Builder::new()
        .prefix(&format!(
            ".{}.migrate-",
            target.file_name().unwrap_or_default().to_string_lossy()
        ))
        .tempdir_in(parent)?;
    copy_dir_contents(legacy, temp.path())?;
    let temp_path = temp.keep();
    match fs::rename(&temp_path, target) {
        Ok(()) => Ok(()),
        Err(_error) if dir_non_empty(target).unwrap_or(false) => {
            let _ = fs::remove_dir_all(temp_path);
            Ok(())
        }
        Err(error) => {
            let _ = fs::remove_dir_all(temp_path);
            Err(error)
        }
    }
}

fn dir_non_empty(path: &Path) -> io::Result<bool> {
    let entries = match fs::read_dir(path) {
        Ok(entries) => entries,
        Err(error) if error.kind() == io::ErrorKind::NotFound => return Ok(false),
        Err(error) => return Err(error),
    };
    for entry in entries {
        let entry = entry?;
        if should_skip_legacy_entry(&entry.file_name().to_string_lossy()) {
            continue;
        }
        let kind = entry.file_type()?;
        if kind.is_file() || (kind.is_dir() && dir_non_empty(&entry.path())?) {
            return Ok(true);
        }
    }
    Ok(false)
}

fn copy_dir_contents(source: &Path, destination: &Path) -> io::Result<()> {
    fs::create_dir_all(destination)?;
    for entry in fs::read_dir(source)? {
        let entry = entry?;
        let name = entry.file_name();
        if should_skip_legacy_entry(&name.to_string_lossy()) {
            continue;
        }
        let kind = entry.file_type()?;
        let target = destination.join(&name);
        if kind.is_symlink() {
            continue;
        }
        if kind.is_dir() {
            copy_dir_contents(&entry.path(), &target)?;
        } else if kind.is_file() {
            copy_file(&entry.path(), &target, entry.metadata()?.permissions())?;
        }
    }
    Ok(())
}

fn should_skip_legacy_entry(name: &str) -> bool {
    name.starts_with("._")
        || name.ends_with(".lock")
        || (name.starts_with('.') && name.contains(".tmp-"))
}

fn copy_file(source: &Path, destination: &Path, permissions: fs::Permissions) -> io::Result<()> {
    if let Some(parent) = destination.parent() {
        fs::create_dir_all(parent)?;
    }
    let mut input = File::open(source)?;
    let mut output = OpenOptions::new()
        .create(true)
        .truncate(true)
        .write(true)
        .open(destination)?;
    let result = (|| {
        io::copy(&mut input, &mut output)?;
        output.flush()?;
        output.sync_all()?;
        fs::set_permissions(destination, permissions)
    })();
    if result.is_err() {
        let _ = fs::remove_file(destination);
    }
    result
}

pub fn read_to_string(path: &Path) -> io::Result<String> {
    let mut value = String::new();
    File::open(path)?.read_to_string(&mut value)?;
    Ok(value)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn migration_copies_files_and_skips_locks_and_symlinks() {
        let root = tempfile::tempdir().unwrap();
        let legacy = root.path().join("legacy");
        let target = root.path().join("state/codex");
        fs::create_dir_all(legacy.join("accounts")).unwrap();
        fs::write(legacy.join("accounts/a.json"), "{}\n").unwrap();
        fs::write(legacy.join(".a.lock"), "lock").unwrap();
        migrate_codex_dir(&target, &legacy).unwrap();
        assert_eq!(
            fs::read_to_string(target.join("accounts/a.json")).unwrap(),
            "{}\n"
        );
        assert!(!target.join(".a.lock").exists());
        assert!(legacy.join("accounts/a.json").exists());
    }

    #[test]
    fn migration_is_safe_when_two_callers_start_together() {
        let root = tempfile::tempdir().unwrap();
        let legacy = root.path().join("legacy");
        let target = root.path().join("state/codex");
        fs::create_dir_all(legacy.join("accounts")).unwrap();
        for name in ["a.json", "b.json", "c.json"] {
            fs::write(legacy.join("accounts").join(name), "{}\n").unwrap();
        }
        std::thread::scope(|scope| {
            let left = scope.spawn(|| migrate_codex_dir(&target, &legacy));
            let right = scope.spawn(|| migrate_codex_dir(&target, &legacy));
            left.join().unwrap().unwrap();
            right.join().unwrap().unwrap();
        });
        for name in ["a.json", "b.json", "c.json"] {
            assert!(target.join("accounts").join(name).is_file());
        }
    }
}
