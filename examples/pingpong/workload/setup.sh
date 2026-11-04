#!/bin/sh
# workload's entrypoint: once pong answers, tell timeskip (and Antithesis)
# that setup is complete, then stay up for the test commands.
until nc -z pong 7000; do sleep 0.1; done
echo '{"antithesis_setup": {"status": "complete", "details": null}}' >> "$ANTITHESIS_OUTPUT_DIR/sdk.jsonl"
exec sleep 2147483647
