#!/bin/sh
# Prints the license notices of everything linked into laertes.exe: the Go runtime and standard
# library, and every module dependency. The MIT and BSD licenses of these require their notices
# to be shipped with the binary, so the release includes this output as THIRD_PARTY_LICENSES.txt.
# Run from the repository root:  GOOS=windows sh scripts/licenses.sh > THIRD_PARTY_LICENSES.txt
set -e
export GOOS="${GOOS:-windows}"

echo "Third-party licenses for laertes.exe"
echo "===================================="
echo
echo "laertes.exe contains the following software. Their license notices follow."
echo
echo "---------------------------------------------------------------------------"
echo "Go standard library and runtime ($(go env GOVERSION))"
echo "---------------------------------------------------------------------------"
cat "$(go env GOROOT)/LICENSE"

go list -deps -f '{{if .Module}}{{if not .Module.Main}}{{.Module.Path}} {{.Module.Version}}{{end}}{{end}}' . |
sort -u | while read -r path version; do
	dir=$(go list -m -f '{{.Dir}}' "$path")
	echo
	echo "---------------------------------------------------------------------------"
	echo "$path $version"
	echo "---------------------------------------------------------------------------"
	for f in "$dir"/LICENSE* "$dir"/COPYING* "$dir"/NOTICE* "$dir"/AUTHORS*; do
		[ -f "$f" ] || continue
		echo "[$(basename "$f")]"
		cat "$f"
		echo
	done
done
