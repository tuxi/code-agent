package session

// This file previously contained the SQLiteStore implementation. It has moved
// to internal/session/sqlite. Import that package and call sqlite.New(path).
//
// The session.Store interface and its sub-interfaces (SessionStore, EventStore,
// TelemetryStore) remain in this package. The sqlite sub-package provides the
// default SQLite-backed implementation.
