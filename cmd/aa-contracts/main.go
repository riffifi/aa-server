// aa-contracts reports implementation coverage against the Python route inventory.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"regexp"

	"aa-server/internal/httpserver"
	"aa-server/internal/identityserver"
)

func run() error {
	input := flag.String("inventory", "internal/contracttest/testdata/routes.json", "Python route inventory")
	output := flag.String("output", "", "optional report output path")
	flag.Parse()
	data, err := os.ReadFile(*input)
	if err != nil {
		return err
	}
	var routes []map[string]any
	if err = json.Unmarshal(data, &routes); err != nil {
		return err
	}
	variables := regexp.MustCompile(`\{[^}]+\}`)
	native := map[string]bool{}
	for _, route := range httpserver.NativeRoutes() {
		native["sessions:"+route.Method+" "+variables.ReplaceAllString(route.Path, "{}")] = true
	}
	for _, route := range identityserver.NativeRoutes() {
		native["aaid:"+route.Method+" "+variables.ReplaceAllString(route.Path, "{}")] = true
	}
	count := 0
	excluded := 0
	for _, route := range routes {
		method, _ := route["method"].(string)
		path, _ := route["path"].(string)
		service, _ := route["service"].(string)
		if service == "sessions" && (path == "/tg/connect" || path == "/site/telegram-link" || path == "/api/accounts/{identifier}/pass-request") {
			route["native_go"] = false
			route["excluded_reason"] = "Telegram disabled by user request"
			excluded++
			continue
		}
		implemented := native[service+":"+method+" "+variables.ReplaceAllString(path, "{}")]
		route["native_go"] = implemented
		if implemented {
			count++
		}
	}
	report := map[string]any{"total": len(routes), "in_scope": len(routes) - excluded, "excluded": excluded, "implemented": count, "remaining": len(routes) - excluded - count, "note": "Handler implementation coverage; Telegram excluded by user request; full behavioral parity is not established.", "routes": routes}
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	if *output != "" {
		if err = os.WriteFile(*output, encoded, 0644); err != nil {
			return err
		}
		fmt.Printf("Go handlers: %d/%d; remaining: %d; excluded: %d\n", count, len(routes)-excluded, len(routes)-excluded-count, excluded)
		return nil
	}
	_, err = os.Stdout.Write(encoded)
	return err
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
