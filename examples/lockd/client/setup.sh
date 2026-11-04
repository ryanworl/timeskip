#!/bin/sh
# client's entrypoint: once lockd answers, tell timeskip (and Antithesis)
# that setup is complete, then stay up for the test commands.
until wget -q -O /dev/null http://lockd:7000/health; do sleep 0.1; done
echo '{"antithesis_setup": {"status": "complete", "details": null}}' >> "$ANTITHESIS_OUTPUT_DIR/sdk.jsonl"
exec sleep 2147483647
