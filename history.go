package main

import (
	"encoding/json"
	"fmt"
	"time"
)

type HistoryEntry struct {
	Timestamp int64  `json:"timestamp"`
	Command   string `json:"command"`
	Value     string `json:"value"`
}

type HTMLHistoryEntry struct {
	When string
	What string
}

func AddHistoryEntry(command, value string) error {
	ts := time.Now().UnixNano()
	historyEntry := HistoryEntry{
		Timestamp: ts,
		Command:   command,
		Value:     value,
	}
	key := BuildHistoryKey(ts)
	b, err := json.Marshal(historyEntry)
	if err != nil {
		return err
	}
	if err := db.Put(key, b); err != nil {
		return err
	}
	return nil
}

func BuildHistoryKey(timestamp int64) []byte {
	return []byte(fmt.Sprintf("history_%d", timestamp))
}
