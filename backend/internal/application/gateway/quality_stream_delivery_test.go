package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

type qualityChunkReader struct {
	io.Reader
	limit int
}

func (r qualityChunkReader) Read(p []byte) (int, error) {
	if len(p) > r.limit {
		p = p[:r.limit]
	}
	return r.Reader.Read(p)
}

// Real network reads can coalesce a complete response or split individual SSE
// lines. Neither layout may turn valid reasoning/tools into a quality strike,
// hide an upstream failure, or change the bytes delivered to the client.
func TestQualityDeliveryIndependentOfReadBoundaries(t *testing.T) {
	t.Parallel()
	longText := strings.Repeat("word ", 40)
	for _, test := range []struct {
		name     string
		protocol string
		frames   []string
		verdict  QualityVerdict
		wantErr  error
	}{
		{
			name: "responses high reasoning ratio", protocol: qualityProtocolResponses, verdict: QualityDeliver,
			frames: []string{
				`data: {"type":"response.reasoning_summary_text.delta","delta":"Check the result."}`,
				`data: {"type":"response.output_text.delta","delta":"OK"}`,
				`data: {"type":"response.completed","response":{"usage":{"output_tokens":1001,"output_tokens_details":{"reasoning_tokens":1000}}}}`,
			},
		},
		{
			name: "cipher before late usage", protocol: qualityProtocolResponses, verdict: QualityDeliver,
			frames: []string{
				`data: {"type":"response.output_item.done","item":{"id":"rs_1","type":"reasoning","encrypted_content":"` + strings.Repeat("A", 300) + `"}}`,
				`data: {"type":"response.output_text.delta","delta":"` + longText + `"}`,
				`data: {"type":"response.completed","response":{"usage":{"output_tokens":11000,"output_tokens_details":{"reasoning_tokens":10000}}}}`,
			},
		},
		{
			name: "summary in terminal output", protocol: qualityProtocolResponses, verdict: QualityDeliver,
			frames: []string{
				`data: {"type":"response.output_text.delta","delta":"` + longText + `"}`,
				`data: {"type":"response.completed","response":{"output":[{"id":"rs_1","type":"reasoning","summary":[{"type":"summary_text","text":"Checked."}]}]}}`,
			},
		},
		{
			name: "reasoning content in done item", protocol: qualityProtocolResponses, verdict: QualityDeliver,
			frames: []string{
				`data: {"type":"response.output_text.delta","delta":"` + longText + `"}`,
				`data: {"type":"response.output_item.done","item":{"id":"rs_1","type":"reasoning","content":[{"type":"reasoning_text","text":"Checked."}]}}`,
				`data: {"type":"response.completed","response":{}}`,
			},
		},
		{
			name: "chat high reasoning ratio", protocol: qualityProtocolChat, verdict: QualityDeliver,
			frames: []string{
				`data: {"choices":[{"delta":{"reasoning_content":"Checked."}}]}`,
				`data: {"choices":[{"delta":{"content":"OK"}}]}`,
				`data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"completion_tokens":1001,"completion_tokens_details":{"reasoning_tokens":1000}}}`,
			},
		},
		{
			name: "responses function call and usage", protocol: qualityProtocolResponses, verdict: QualityDeliver,
			frames: []string{
				`data: {"type":"response.output_text.delta","delta":"` + longText + `"}`,
				`data: {"type":"response.output_item.added","item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"read_file","arguments":""}}`,
				`data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{\"path\":\"E:\\\\work\\\\x.txt\"}"}`,
				`data: {"type":"response.completed","response":{"usage":{"output_tokens":1100,"output_tokens_details":{"reasoning_tokens":1000}}}}`,
			},
		},
		{
			name: "terminal function call and usage", protocol: qualityProtocolResponses, verdict: QualityDeliver,
			frames: []string{
				`data: {"type":"response.completed","response":{"output":[{"type":"function_call","call_id":"call_1","name":"read_file","arguments":"{}"}],"usage":{"output_tokens":200}}}`,
			},
		},
		{
			name: "custom tool call and usage", protocol: qualityProtocolResponses, verdict: QualityDeliver,
			frames: []string{
				`data: {"type":"response.custom_tool_call_input.delta","delta":"test input"}`,
				`data: {"type":"response.completed","response":{"usage":{"output_tokens":200}}}`,
			},
		},
		{
			name: "chat tool call and usage", protocol: qualityProtocolChat, verdict: QualityDeliver,
			frames: []string{
				`data: {"choices":[{"delta":{"tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"completion_tokens":200}}`,
			},
		},
		{
			name: "anthropic tool use and usage", protocol: qualityProtocolAnthropic, verdict: QualityDeliver,
			frames: []string{
				`data: {"type":"content_block_start","content_block":{"type":"tool_use","id":"tool_1","name":"read_file","input":{}}}`,
				`data: {"type":"message_delta","usage":{"output_tokens":200}}`,
				`data: {"type":"message_stop"}`,
			},
		},
		{
			name: "upstream failed after text", protocol: qualityProtocolResponses, verdict: QualityDeliver,
			frames: []string{
				`data: {"type":"response.output_text.delta","delta":"` + longText + `"}`,
				`data: {"type":"response.failed","response":{"error":{"code":"upstream_stream_idle_timeout","message":"idle"},"usage":{"output_tokens":100}}}`,
			},
		},
		{
			name: "empty upstream failed", protocol: qualityProtocolResponses, verdict: QualityDeliver,
			frames: []string{`data: {"type":"response.failed","response":{"error":{"code":"upstream_unavailable"}}}`},
		},
		{
			name: "upstream incomplete", protocol: qualityProtocolResponses, verdict: QualityDeliver,
			frames: []string{`data: {"type":"response.incomplete","response":{"incomplete_details":{"reason":"max_output_tokens"}}}`},
		},
		{
			name: "chat upstream error", protocol: qualityProtocolChat, verdict: QualityDeliver,
			frames: []string{`data: {"error":{"code":"upstream_unavailable","message":"unavailable"}}`},
		},
		{
			name: "anthropic upstream error", protocol: qualityProtocolAnthropic, verdict: QualityDeliver,
			frames: []string{`data: {"type":"error","error":{"type":"api_error","message":"unavailable"}}`},
		},
		{
			name: "usage without output is empty", protocol: qualityProtocolResponses, verdict: QualityWait, wantErr: errQualityEmptyStream,
			frames: []string{`data: {"type":"response.completed","response":{"output":[],"usage":{"output_tokens":100}}}`},
		},
		{
			name: "terminal text without requested reasoning", protocol: qualityProtocolResponses, verdict: QualityWithhold,
			frames: []string{
				`data: {"type":"response.output_text.delta","delta":"` + longText + `"}`,
				`data: {"type":"response.completed","response":{}}`,
			},
		},
	} {
		for _, chunkSize := range []int{1, 7, 4096} {
			t.Run(fmt.Sprintf("%s/chunk_%d", test.name, chunkSize), func(t *testing.T) {
				t.Parallel()
				for _, frame := range test.frames {
					if !json.Valid([]byte(strings.TrimPrefix(frame, "data: "))) {
						t.Fatalf("invalid synthetic SSE fixture: %s", frame)
					}
				}
				wire := sse(test.frames...)
				body := io.NopCloser(qualityChunkReader{Reader: strings.NewReader(wire), limit: chunkSize})
				replay, verdict, _, _, err := peekQualityStream(context.Background(), body, test.protocol, QualityRetryRuntime{MinOutputTokens: 32, HoldTimeout: 10 * time.Second})
				if replay != nil {
					defer replay.Close()
				}
				if !errors.Is(err, test.wantErr) || verdict != test.verdict {
					t.Fatalf("verdict=%s err=%v; want %s %v", verdict, err, test.verdict, test.wantErr)
				}
				if replay == nil {
					t.Fatal("missing original response body")
				}
				got, readErr := io.ReadAll(replay)
				if readErr != nil || string(got) != wire {
					t.Fatalf("response body changed or failed to drain: %v", readErr)
				}
			})
		}
	}
}
