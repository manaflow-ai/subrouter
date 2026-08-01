//! Subrouter library.

pub mod account;
pub mod accounts;
pub mod agents;
pub mod autoswitch;
pub mod broker;
pub mod cli;
pub mod cloud;
pub mod front;
mod local_commands;
pub mod proxy;
pub mod remote;
pub mod selectacct;
pub mod servers;
pub mod service;
pub mod session;
pub mod storepath;
pub mod supervisor;
pub mod tenant;
pub mod transcript;

#[cfg(test)]
mod distribution_tests {
    use std::fs;
    use std::path::Path;

    use regex::Regex;
    use serde_json::Value;
    use toml_edit::DocumentMut;

    #[test]
    fn distribution_versions_and_toolchain_match_the_crate() {
        let root = Path::new(env!("CARGO_MANIFEST_DIR"));
        let version = env!("CARGO_PKG_VERSION");
        let rust_version = env!("CARGO_PKG_RUST_VERSION");

        let package: Value = serde_json::from_str(
            &fs::read_to_string(root.join("package.json")).expect("read package.json"),
        )
        .expect("parse package.json");
        assert_eq!(package["version"].as_str(), Some(version));

        let pyproject: DocumentMut = fs::read_to_string(root.join("pyproject.toml"))
            .expect("read pyproject.toml")
            .parse()
            .expect("parse pyproject.toml");
        assert_eq!(pyproject["project"]["version"].as_str(), Some(version));

        let python = fs::read_to_string(root.join("subrouter_cli/__init__.py"))
            .expect("read Python wrapper");
        let wrapper_version = Regex::new(r#"VERSION = "([^"]+)""#)
            .unwrap()
            .captures(&python)
            .and_then(|captures| captures.get(1))
            .map(|value| value.as_str());
        assert_eq!(wrapper_version, Some(version));

        let toolchain: DocumentMut = fs::read_to_string(root.join("rust-toolchain.toml"))
            .expect("read rust-toolchain.toml")
            .parse()
            .expect("parse rust-toolchain.toml");
        assert_eq!(
            toolchain["toolchain"]["channel"].as_str(),
            Some(rust_version)
        );
    }
}
