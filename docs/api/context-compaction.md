# Conversation context compaction

`POST /v1/conversations/{id}/compact` compacts model context without creating a
user turn or executing tools. The existing transcript event log remains intact.
It uses the conversation's model and session credential. Active, queued, paused,
and background-waiting conversations cannot be compacted concurrently with a
turn; the endpoint returns HTTP 409. Unknown conversations return 404 and runtimes
without compaction support return 501. Summarization failures leave stored context
unchanged. The operation has a five-minute deadline.

The standard response envelope contains:

```json
{"status":"compacted_pending_measurement","snapshot":{}}
```

`snapshot` has the same shape as `GET /v1/conversations/{id}/context`. `status` is
`no_change` when no older context could be folded. A successful compaction has
`compaction.last.after_tokens = -1` until the next successful model request
measures the new input. `current.prompt_tokens` remains the last measured usage;
it is not a post-compaction measurement. Pending statistics do not contribute to
cumulative saved tokens.

The snapshot model includes `output_token_budget` and `input_token_budget`.
Generation is capped at min(16,384, window / 8), and the input budget additionally
reserves 5% of the window. Chat Completions sends this cap as `max_tokens` and
Responses sends `max_output_tokens`. Text and echoed reasoning are included in
runtime estimates. Estimates cannot guarantee a provider's exact tokenizer count;
a provider context rejection permits one forced-compaction retry per turn before
surfacing failure. Recovery never replays a partial streamed answer.

Automatic compaction retains cache cooldown away from the input budget boundary.
Near that boundary it bypasses cooldown. Summary requests fold bounded text chunks
into a cumulative digest, with an explicit output budget; any failed chunk or
truncated summary aborts the context replacement.
