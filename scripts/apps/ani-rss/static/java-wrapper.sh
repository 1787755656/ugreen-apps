#!/bin/sh
# UGOS: force UTF-8 filesystem encoding before HotSpot initializes jnuEncoding.
DIR=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$DIR/../.." && pwd)

# Bundled glibc locale (from Debian C.utf8)
if [ -d "$ROOT/locale" ]; then
  export LOCPATH="$ROOT/locale"
fi
export LANG=C.UTF-8
export LC_ALL=C.UTF-8
export LC_CTYPE=C.UTF-8
export LANGUAGE=C.UTF-8

# Early JVM options (applied by launcher before main)
# Keep existing if any, prepend our force flags
_ENC_OPTS="-Dsun.jnu.encoding=UTF-8 -Dfile.encoding=UTF-8 -Dnative.encoding=UTF-8 -Dstdout.encoding=UTF-8 -Dstderr.encoding=UTF-8"
export JDK_JAVA_OPTIONS="${_ENC_OPTS} ${JDK_JAVA_OPTIONS:-}"
export JAVA_TOOL_OPTIONS="${_ENC_OPTS} ${JAVA_TOOL_OPTIONS:-}"

exec "$DIR/java.real" "$@"
