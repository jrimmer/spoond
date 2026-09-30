if [ -z "$TMUX" ] && [ -z "${SPOOND_NO_TMUX:-}" ] && [ -z "${FORKD_NO_TMUX:-}" ] && [ -n "${SSH_CONNECTION:-}" ]; then
  case "$TERM" in
    xterm-kitty|alacritty|wezterm|dumb) export TERM=xterm-256color ;;
  esac
  if tmux new -A -s dev 2>/dev/null; then
    exit 0
  fi
  echo "spoond: tmux unavailable (TERM=$TERM) — plain shell"
fi
