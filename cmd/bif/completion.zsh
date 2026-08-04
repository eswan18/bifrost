#compdef bif

# zsh completion for bif, printed by `bif completion zsh`.
#
# Every candidate comes from the binary itself, so this file never goes stale:
# the fleet, the flags and the live preview tags are whatever the installed bif
# says they are. Install it with:
#
#     bif completion zsh > "${fpath[1]}/_bif"    # then restart the shell
#     source <(bif completion zsh)               # or, for this shell only
#
# compinit must have run first, since that is what defines compdef.

_bif() {
  local -a candidates
  # words[2,CURRENT] is everything after "bif" up to and including the word
  # being typed, which is the empty string when the cursor sits after a space.
  # (@) keeps that empty word, because its position is what tells bif which
  # argument is being completed. bif prefix-filters what it returns, so there
  # is nothing to match on here.
  #
  # stderr is discarded: it cannot become a candidate — bif __complete writes
  # only candidates, and only to stdout — but a bif missing from PATH would
  # otherwise scribble "command not found" over the prompt on every Tab.
  candidates=(${(f)"$(bif __complete "${(@)words[2,CURRENT]}" 2>/dev/null)"})
  (( ${#candidates} )) && compadd -- ${candidates}
}

if [[ ${funcstack[1]} == _bif ]]; then
  # Autoloaded from fpath: compinit runs this file to define the function and
  # expects the first completion out of the same call, so run what was just
  # defined.
  _bif "$@"
else
  compdef _bif bif
fi
