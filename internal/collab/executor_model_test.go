package collab

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func TestExecutorModelSharedJSONContract(t *testing.T) {
	raw,err := os.ReadFile("../../docs/contracts/task-executor-model.json"); if err != nil { t.Fatal(err) }
	var in StartRunInput
	if err := json.Unmarshal(raw,&in); err != nil { t.Fatal(err) }
	if in.ExecutorModel != "opencode-go/qwen3.8-flash" || in.MaxIterations != 2 { t.Fatal(in) }
	encoded,err := json.Marshal(in); if err != nil { t.Fatal(err) }
	var a,b any
	if err = json.Unmarshal(raw,&a); err != nil { t.Fatal(err) }
	if err = json.Unmarshal(encoded,&b); err != nil { t.Fatal(err) }
	if !reflect.DeepEqual(a,b) { t.Fatal("contract fields changed") }
}
