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

echo
echo "---------------------------------------------------------------------------"
echo "Coding recipes from vwcoding.ru (github.com/Kanaduchi/vwcoding), embedded in index.html"
echo "---------------------------------------------------------------------------"
cat <<'EOF'
MIT License

Copyright (c) 2026 vwcoding.ru (info@vwcoding.ru)

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
EOF

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
