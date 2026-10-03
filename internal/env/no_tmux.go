package env

// NoTmux reports whether the guest should skip the tmux auto-attach on
// SSH login. It mirrors the precedence in
// images/guest/spoond-tmux.sh: SPOOND_NO_TMUX wins, and the deprecated
// pre-2.0 FORKD_NO_TMUX name still works, logging the same one-line
// deprecation warning once per process as every other renamed
// variable.
func NoTmux() bool {
	return Get("SPOOND_NO_TMUX", "") != ""
}
