# tmux auto-attach for interactive SSH logins. SPOOND_NO_TMUX opts out;
# the pre-2.0 FORKD_NO_TMUX name still works (deprecated, warns once).
if [ -z "$TMUX" ] && [ -n "${SSH_CONNECTION:-}" ]; then
  no_tmux=${SPOOND_NO_TMUX:-}
  if [ -z "$no_tmux" ] && [ -n "${FORKD_NO_TMUX:-}" ]; then
    no_tmux=$FORKD_NO_TMUX
    # Deprecation warning, once per user: each login shell is its own
    # process, so a canary file in $HOME dedupes across logins. If the
    # canary cannot be written, warn anyway — nagging beats silence.
    warned="${HOME:-/tmp}/.spoond-forkd-no-tmux-warned"
    if [ ! -e "$warned" ]; then
      echo "spoond: FORKD_NO_TMUX is deprecated: use SPOOND_NO_TMUX (the old name still works in 2.0)" >&2
      mkdir -p -- "$(dirname -- "$warned")" 2>/dev/null \
        && (: > "$warned") 2>/dev/null || true
    fi
  fi
  if [ -z "$no_tmux" ]; then
    case "$TERM" in
      xterm-kitty|alacritty|wezterm|dumb) export TERM=xterm-256color ;;
    esac
    if tmux new -A -s dev 2>/dev/null; then
      exit 0
    fi
    echo "spoond: tmux unavailable (TERM=$TERM) — plain shell"
  fi
fi
