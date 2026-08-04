# bash completion for bif, printed by `bif completion bash`.
#
# Every candidate comes from the binary itself, so this file never goes stale:
# the fleet, the flags and the live preview tags are whatever the installed bif
# says they are. Install it with:
#
#     bif completion bash > ~/.local/share/bash-completion/completions/bif
#     source <(bif completion bash)               # or, for this shell only
#
# Kept to bash 3.2 syntax — that is what macOS ships, and a shim that needs
# mapfile would fail there and nowhere else.

_bif() {
    local line
    COMPREPLY=()
    # "${COMP_WORDS[@]:1:COMP_CWORD}" is everything after "bif" up to and
    # including the word being typed, which is the empty string when the cursor
    # sits after a space. That empty word is kept deliberately: its position is
    # what tells bif which argument is being completed. bif prefix-filters what
    # it returns, so compgen has nothing to add.
    #
    # read -r into the array rather than $(...) word splitting, so a candidate
    # is never re-split or glob-expanded on its way to the shell. stderr is
    # discarded: it cannot become a candidate — bif __complete writes only
    # candidates, and only to stdout — but a bif missing from PATH would
    # otherwise scribble "command not found" over the prompt on every Tab.
    while IFS= read -r line; do
        COMPREPLY+=("$line")
    done < <(bif __complete "${COMP_WORDS[@]:1:COMP_CWORD}" 2>/dev/null)
}

complete -F _bif bif
