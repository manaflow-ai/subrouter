#!/usr/bin/env bash
# Feed a shell value through a concurrent process-substitution stream. Bash
# 5.1+ may implement here-strings with a pipe that it fills before starting the
# reader, which can deadlock inside command substitutions on macOS.

stream_shell_value() {
  if (( $# != 1 )); then
    echo "stream_shell_value requires exactly one value" >&2
    return 2
  fi
  printf '%s\n' "$1"
}
