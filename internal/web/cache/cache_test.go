package cache

import (
	"testing"
	"time"
)

func TestCacheHit(t *testing.T) {
	c := New(10 * time.Second)
	c.Store("https://example.com", []byte("hello"), "text/html")

	e, ok := c.Load("https://example.com")
	if !ok {
		t.Fatal("expected cache hit")
	}
	if string(e.Body) != "hello" {
		t.Fatalf("expected body 'hello', got %q", e.Body)
	}
	if e.ContentType != "text/html" {
		t.Fatalf("expected content-type 'text/html', got %q", e.ContentType)
	}
}

func TestCacheMiss(t *testing.T) {
	c := New(10 * time.Second)
	_, ok := c.Load("https://example.com/missing")
	if ok {
		t.Fatal("expected cache miss")
	}
}

func TestCacheExpiry(t *testing.T) {
	c := New(10 * time.Millisecond)
	c.Store("https://example.com", []byte("hello"), "text/html")

	time.Sleep(15 * time.Millisecond)

	_, ok := c.Load("https://example.com")
	if ok {
		t.Fatal("expected cache miss due to expiry")
	}
}

func TestCacheDisabled(t *testing.T) {
	c := New(0)
	c.Store("https://example.com", []byte("hello"), "text/html")

	_, ok := c.Load("https://example.com")
	if ok {
		t.Fatal("expected cache miss when TTL is 0")
	}
}
