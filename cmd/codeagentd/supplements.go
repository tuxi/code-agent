package main

import (
	"code-agent/internal/conversation"
	"code-agent/internal/session"
	sqlitestore "code-agent/internal/session/sqlite"
	"fmt"
)

func configureExperimentalSupplements(executor *conversation.TurnExecutor, store session.Store, enabled bool) error {
	if !enabled {
		return nil
	}
	sqlite, ok := store.(*sqlitestore.Store)
	if !ok {
		return fmt.Errorf("experimental text supplements require the SQLite store")
	}
	inbox, err := sqlitestore.NewSupplementInbox(sqlite)
	if err != nil {
		return fmt.Errorf("open supplement inbox: %w", err)
	}
	executor.SetTurnDeliveryInbox(inbox)
	return nil
}
