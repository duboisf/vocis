#!/bin/bash
#
# Manually exercise the Lemonade post-processing path with lfm2.5-it-1.2b-FLM.
# Prints the raw transcript then streams the cleaned output.

set -euo pipefail

MODEL="${MODEL:-lfm2.5-it-1.2b-FLM}"
LEMONADE_URL="${LEMONADE_URL:-http://localhost:13305/api/v1}"

MESSAGES=$(cat <<'EOF'
[
  {
    "role": "system",
    "content": "You are a transcription cleanup filter, not a chatbot. You receive a raw dictation transcript and output a cleaned version of the same sentence in the same voice.\n\nHARD RULES:\n- Preserve first-person exactly. If the speaker says 'I' or 'we', keep 'I' or 'we'. Never flip to 'you' or 'they'.\n- Preserve the statement/question form. A statement stays a statement. Never append 'right?', 'correct?', or any confirmation tag. Never turn a statement into a question directed at the speaker.\n- Do not address, acknowledge, summarize, or paraphrase the speaker. You are not having a conversation with them.\n- Remove filler words (um, uh, like, you know, I mean, sort of, kind of), false starts, stutters, duplicated words, and trailing pauses.\n- Light rephrasing for grammar and flow is allowed, but subject, verb tense, and pronouns must match the original.\n- Do not add information. Do not answer questions. If the input is a question, output the same question.\n\nEXAMPLE:\nInput: 'so um i was thinking like maybe we could refactor the the auth stuff i guess'\nOutput: I was thinking maybe we could refactor the auth stuff.\n\nReturn ONLY the cleaned transcription. No preamble, no commentary, no quotes around the output."
  },
  {
    "role": "user",
    "content": "so um i was thinking like maybe we could uh you know sort of refactor the the auth middleware because i mean it's kind of you know getting pretty messy and uh the session token storage is like not really you know compliant with the new rules so um yeah we should probably just rip it out i guess and and start over"
  }
]
EOF
)

echo "Model: $MODEL"
echo
echo "Before:"
jq -r '.[1].content' <<<"$MESSAGES"
echo
echo "After:"
curl -sN "$LEMONADE_URL/chat/completions" \
  -H "Content-Type: application/json" \
  -d "$(jq -c --arg model "$MODEL" '{model:$model, stream:true, messages:.}' <<<"$MESSAGES")" | \
jq -jRn --unbuffered '
  inputs
  | select(startswith("data: "))
  | ltrimstr("data: ")
  | select(. != "[DONE]")
  | fromjson
  | .choices[0].delta | (.content // .reasoning_content // "")
'
echo
