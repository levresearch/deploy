package main

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
)

var stateWriteRequirements = [][2]string{
	{"GNU coreutils (sync)", "sync --version"},
	{"GNU coreutils (mv)", "mv --version"},
	{"GNU coreutils (mktemp)", "mktemp --version"},
}

// verify the complete transfer before replacing state; ssh can end stdin early.
const atomicStateWrite = `set -eu
umask 077
target=$1
expected=$2
directory=$(dirname -- "$target")
name=$(basename -- "$target")
cd -- "$directory"
directory=$(pwd -P)
test ! -L "$name"
if test -e "$name"; then test -f "$name"; fi
temporary=$(mktemp -- .deploy-state.XXXXXXXXXX)
trap 'rm -f -- "$temporary"' EXIT
trap 'exit 1' HUP INT TERM
cat > "$temporary"
test "$(wc -c < "$temporary")" -eq "$expected"
sync -- "$temporary"
mv -fT -- "$temporary" "$name"
while :; do
  sync -- "$directory"
  test "$directory" != / || break
  directory=$(dirname -- "$directory")
done
`

func writeStateFile(runner Runner, filename string, contents []byte) error {
	if filename == "" || strings.ContainsAny(filename, "\x00\r\n") {
		return fmt.Errorf("invalid deployment state path")
	}
	command := []string{"sh", "-c", atomicStateWrite, "deploy-state", filename, strconv.Itoa(len(contents))}
	if err := runner.Pipe(command, bytes.NewReader(contents)); err != nil {
		return fmt.Errorf("cannot durably write deployment state on %s; require GNU coreutils and writable storage; state may already have changed, inspect it before retrying", runner.Describe())
	}
	return nil
}
