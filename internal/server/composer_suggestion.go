package server

import (
 "context"
 "errors"
 "net/http"
 "sync"
 "time"

 "code-agent/internal/conversation"
)

type composerSuggestionEntry struct{
 done chan struct{}
 value conversation.ComposerSuggestion
 err error
}

func registerComposerSuggestionRoute(mux *http.ServeMux,repo conversation.ConversationRepository,executor *conversation.TurnExecutor,opts MuxOptions){
 var mu sync.Mutex
 entries:=map[string]*composerSuggestionEntry{}
 mux.HandleFunc("POST /v1/conversations/{id}/composer/suggestion",func(w http.ResponseWriter,r *http.Request){
  if !managementAuthReady(w,r,opts.ServerAuth){return}
  if executor==nil || repo==nil{ErrorSimple(w,r,http.StatusNotImplemented,"suggestions unavailable");return}
  var req struct{TurnID string `json:"turn_id"`}
  if decodeBoundedJSON(w,r,&req,4096)!=nil || req.TurnID=="" || len(req.TurnID)>256{ErrorSimple(w,r,http.StatusBadRequest,"turn_id is required");return}
  id:=r.PathValue("id")
  if _,err:=loadConversationHeader(r.Context(),repo,id);err!=nil{ErrorSimple(w,r,http.StatusNotFound,"conversation not found");return}
  key:=id+"\x00"+req.TurnID
  mu.Lock()
  entry:=entries[key]
  if entry==nil{
   if len(entries)>=128{
    for k,existing:=range entries{select{case <-existing.done:delete(entries,k);default:}}
   }
   if len(entries)>=128{mu.Unlock();ErrorSimple(w,r,http.StatusServiceUnavailable,"suggestion generation busy");return}
   entry=&composerSuggestionEntry{done:make(chan struct{})};entries[key]=entry
   // Generation is coalesced by source turn and outlives a client changing tabs.
   go func(){
    ctx,cancel:=context.WithTimeout(context.Background(),20*time.Second);defer cancel()
    entry.value,entry.err=executor.ComposerSuggestion(ctx,id,req.TurnID)
    close(entry.done)
   }()
  }
  mu.Unlock()
  select{case <-r.Context().Done():return;case <-entry.done:}
  if entry.err!=nil{
   status:=http.StatusServiceUnavailable
   if errors.Is(entry.err,conversation.ErrComposerSuggestionUnavailable){status=http.StatusNotImplemented}
   if errors.Is(entry.err,conversation.ErrComposerSuggestionStale){status=http.StatusConflict}
   ErrorSimple(w,r,status,"suggestion unavailable");return
  }
  if !executor.ComposerSuggestionCurrent(r.Context(),entry.value){ErrorSimple(w,r,http.StatusConflict,"suggestion source changed");return}
  writeJSON(w,r,http.StatusOK,entry.value)
 })
}
