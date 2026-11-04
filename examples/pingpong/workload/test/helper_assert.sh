# shellcheck shell=sh
# Antithesis SDK assertions, for shell: assert always|sometimes MESSAGE
# CONDITION appends one to $ANTITHESIS_OUTPUT_DIR/sdk.jsonl (see Antithesis's
# fallback SDK).
assert() {
	case $1 in always) d=Always ;; sometimes) d=Sometimes ;; esac
	echo "{\"antithesis_assert\": {\"assert_type\": \"$1\", \"display_type\": \"$d\", \"message\": \"$2\", \"id\": \"$2\", \"condition\": $3, \"hit\": true, \"must_hit\": true, \"details\": null}}" >> "$ANTITHESIS_OUTPUT_DIR/sdk.jsonl"
}
