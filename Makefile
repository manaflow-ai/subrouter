.PHONY: test check lint run build build-linux accounts mock-upstream

test:
	cargo test --locked --all-targets --all-features

check:
	cargo fmt --all --check
	cargo check --locked --all-targets --all-features

lint:
	cargo clippy --locked --all-targets --all-features -- -D warnings

run:
	cargo run --locked --bin subrouter -- serve

build:
	cargo build --locked --bins
	mkdir -p bin
	cp target/debug/subrouter bin/subrouter
	cp target/debug/mockupstream bin/mockupstream
	@if [ "$$(uname -s)" = "Darwin" ]; then codesign -s - -f bin/subrouter bin/mockupstream; fi

build-linux:
	cargo build --locked --release --target x86_64-unknown-linux-musl --bin subrouter
	mkdir -p bin
	cp target/x86_64-unknown-linux-musl/release/subrouter bin/subrouter-linux-amd64

accounts:
	cargo run --locked --bin subrouter -- accounts

mock-upstream:
	cargo run --locked --bin mockupstream
