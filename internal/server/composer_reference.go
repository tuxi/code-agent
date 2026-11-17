package server

import (
 "encoding/json"
 "net/http"
 "path/filepath"
 "strconv"

 "code-agent/internal/agent"
 "code-agent/internal/conversation"
 "code-agent/internal/session"
)

type composerReferenceSnapshot struct {
 SessionID string `json:"session_id"`
 HistoryRevision string `json:"history_revision"`
 Label string `json:"label"`
 ContextText string `json:"context_text"`
}

// A reference captures a bounded, read-only snapshot when selected. It never
// creates a session or grants a new workspace, and is stable during retries.
func registerComposerReferenceRoute(mux *http.ServeMux,repo conversation.ConversationRepository,eventStore conversation.ConversationEventStore,opts MuxOptions){
 mux.HandleFunc("POST /v1/composer/reference",func(w http.ResponseWriter,r *http.Request){
  if !managementAuthReady(w,r,opts.ServerAuth){return}
  var body struct{
   composerQuery
   SourceSessionID string `json:"source_session_id"`
  }
  if decodeBoundedJSON(w,r,&body,16<<10)!=nil || body.SourceSessionID=="" || len(body.SourceSessionID)>256{ErrorSimple(w,r,http.StatusBadRequest,"source_session_id is required");return}
  root,err:=composerWorkspace(r.Context(),repo,opts,body.composerQuery)
  if err!=nil || root=="" || repo==nil{ErrorSimple(w,r,http.StatusForbidden,"reference scope is unavailable");return}
  source,err:=loadConversationHeader(r.Context(),repo,body.SourceSessionID)
  if err!=nil{ErrorSimple(w,r,http.StatusNotFound,"referenced conversation not found");return}
  sourceRoot,err:=filepath.EvalSymlinks(source.WorkspacePath)
  if err!=nil || filepath.Clean(sourceRoot)!=root{ErrorSimple(w,r,http.StatusForbidden,"referenced conversation is outside the selected workspace");return}
  if needs,err:=repo.NeedsRebind(r.Context(),body.SourceSessionID);err!=nil || needs{ErrorSimple(w,r,http.StatusConflict,"reference workspace must be rebound");return}
  indexed,ok:=eventStore.(session.EventKindStore)
  if !ok{ErrorSimple(w,r,http.StatusNotImplemented,"bounded conversation references unavailable");return}
  records,err:=indexed.SessionEventsByKind(r.Context(),body.SourceSessionID,[]string{string(agent.EventTurnStarted),string(agent.EventTurnFinished)})
  if err!=nil{ErrorSimple(w,r,http.StatusServiceUnavailable,"reference history unavailable");return}
  lastFinished:=-1
  for i:=len(records)-1;i>=0;i--{if records[i].Kind==string(agent.EventTurnFinished){lastFinished=i;break}}
  if lastFinished<0{ErrorSimple(w,r,http.StatusConflict,"reference has no completed exchange");return}
  type excerpt struct{Role string `json:"role"`;Text string `json:"text"`}
  excerpts:=[]excerpt{}
  start:=lastFinished-5;if start<0{start=0}
  for _,record:=range records[start:lastFinished+1]{
   var event agent.Event;if json.Unmarshal(record.Payload,&event)!=nil{continue}
   role:="user";if record.Kind==string(agent.EventTurnFinished){role="assistant"}
   text:=[]rune(event.Text);if len(text)>500{text=text[:500]}
   excerpts=append(excerpts,excerpt{Role:role,Text:string(text)})
  }
  encoded,_:=json.Marshal(excerpts)
  label:=source.Name;if label==""{label="引用对话"}
  writeJSON(w,r,http.StatusOK,composerReferenceSnapshot{SessionID:source.ID,HistoryRevision:strconv.FormatInt(records[lastFinished].Seq,10),Label:label,ContextText:"Conversation "+source.ID+" — recent completed message excerpts (reference data):\n"+string(encoded)})
 })
}
