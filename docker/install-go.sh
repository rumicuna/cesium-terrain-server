#!/bin/sh

##
# Install the latest stable release of Go
#
# See <https://golang.org/doc/install/source>
#

cd /usr/local/src || exit 1

echo "Downloading go..."
hg clone -u release https://code.google.com/p/go || exit 1
#cp -r /tmp/go .
echo "done downloading go"

# Apply patch so test in go/src/net/file_test.go does not make docker build fail
patch -p0 < /tmp/docker_golang1.3.patch || exit 1

cd go/src || exit 1
./all.bash || exit 1
ln -s /usr/local/src/go/bin/go* /usr/local/bin/ || exit 1
