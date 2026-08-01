fn main() {
    prepare_legacy_listener_environment();
    let runtime = match tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()
    {
        Ok(runtime) => runtime,
        Err(error) => {
            eprintln!("subrouter: create Tokio runtime: {error}");
            std::process::exit(1);
        }
    };
    if let Err(error) = runtime.block_on(subrouter::cli::main_entry()) {
        eprintln!("subrouter: {error:#}");
        std::process::exit(1);
    }
}

/// Go supervisor releases pass a worker Unix listener in this legacy variable.
/// Translate it before Tokio starts any threads so listenfd can validate and
/// consume the descriptor through the systemd-compatible protocol.
#[cfg(unix)]
fn prepare_legacy_listener_environment() {
    let Ok(first_fd) = std::env::var("SUBROUTER_LISTEN_FD") else {
        return;
    };
    if first_fd.trim().parse::<i32>().is_err() || std::env::var_os("LISTEN_FDS").is_some() {
        return;
    }
    // SAFETY: main calls this before constructing the multi-threaded runtime,
    // so no other thread can concurrently access the process environment.
    #[allow(unsafe_code)]
    unsafe {
        std::env::set_var("LISTEN_FDS", "1");
        std::env::set_var("LISTEN_FDS_FIRST_FD", first_fd);
    }
}

#[cfg(not(unix))]
fn prepare_legacy_listener_environment() {}
