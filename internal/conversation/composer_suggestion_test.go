package conversation

import (
 "context"
 "encoding/json"
 "errors"
 "code-agent/internal/agent"
 "code-agent/internal/session"
 "strings"
 "testing"
 "unicode/utf8"
)

func TestComposerSuggestionCarriesSessionIdentityAndBoundsUnicode(t *testing.T){
 provider:=&titledProvider{reply:strings.Repeat("继续检查输入框",80)}
 generator:=NewLLMTitleGenerator(provider,"auxiliary-model")
 text,err:=generator.GenerateComposerSuggestion(context.Background(),"session-1","请优化输入框","已完成输入框基础布局")
 if err!=nil{t.Fatal(err)}
 if provider.lastReq.SessionID!="session-1" || provider.lastReq.Model!="auxiliary-model"{t.Fatalf("wrong request identity: %+v",provider.lastReq)}
 if !utf8.ValidString(text) || len([]rune(text))>240{t.Fatal("suggestion was not bounded on Unicode boundaries")}
 if len(provider.lastReq.Tools)!=0{t.Fatal("suggestions must not expose tools")}
}

type indexedSuggestionEvents struct{fakeEventStore}
func(s *indexedSuggestionEvents)SessionEventsByKind(ctx context.Context,id string,kinds []string)([]session.EventRecord,error){
 records,err:=s.Replay(ctx,id);if err!=nil{return nil,err};out:=[]session.EventRecord{}
 for _,record:=range records{for _,kind:=range kinds{if record.Kind==kind{out=append(out,record);break}}};return out,nil
}
type suggestionGeneratorStub struct{calls int;onGenerate func()}
func(g *suggestionGeneratorStub)GenerateTitle(context.Context,string,string,string)(string,error){return "",nil}
func(g *suggestionGeneratorStub)GenerateComposerSuggestion(context.Context,string,string,string)(string,error){g.calls++;if g.onGenerate!=nil{g.onGenerate()};return "继续验证输入框",nil}
func appendSuggestionEvent(t *testing.T,store *indexedSuggestionEvents,kind agent.EventKind,turnID,text string){
 t.Helper();payload,err:=json.Marshal(agent.Event{Kind:kind,TurnID:turnID,Text:text});if err!=nil{t.Fatal(err)}
 if _,err:=store.Append(context.Background(),session.EventRecord{SessionID:"session",Kind:string(kind),Payload:payload});err!=nil{t.Fatal(err)}
}
func TestComposerSuggestionRejectsStaleSourcesBeforeAndAfterGeneration(t *testing.T){
 store:=&indexedSuggestionEvents{};generator:=&suggestionGeneratorStub{}
 executor:=&TurnExecutor{events:store,titleGen:generator}
 appendSuggestionEvent(t,store,agent.EventTurnStarted,"turn-1","优化输入框")
 appendSuggestionEvent(t,store,agent.EventTurnFinished,"turn-1","输入框已更新")
 suggestion,err:=executor.ComposerSuggestion(context.Background(),"session","turn-1");if err!=nil || suggestion.Text==""{t.Fatalf("suggestion=%+v err=%v",suggestion,err)}
 if !executor.ComposerSuggestionCurrent(context.Background(),suggestion){t.Fatal("current suggestion rejected")}
 appendSuggestionEvent(t,store,agent.EventTurnStarted,"turn-2","开始下一轮")
 if executor.ComposerSuggestionCurrent(context.Background(),suggestion){t.Fatal("cached suggestion survived a new turn")}
 calls:=generator.calls
 if _,err:=executor.ComposerSuggestion(context.Background(),"session","turn-1");!errors.Is(err,ErrComposerSuggestionStale){t.Fatalf("stale source: %v",err)}
 if generator.calls!=calls{t.Fatal("stale source spent a provider request")}
 appendSuggestionEvent(t,store,agent.EventTurnFinished,"turn-2","完成")
 generator.onGenerate=func(){appendSuggestionEvent(t,store,agent.EventTurnStarted,"turn-3","新请求")}
 if _,err:=executor.ComposerSuggestion(context.Background(),"session","turn-2");!errors.Is(err,ErrComposerSuggestionStale){t.Fatalf("late suggestion: %v",err)}
}
