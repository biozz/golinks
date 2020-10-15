#!/bin/sh

PORT="${PORT:-8000}"

exec /golinks -bind=0.0.0.0:"$PORT" "$@"
