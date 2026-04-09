package reroute

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
)

func ExampleReRouter() {
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer httpServer.Close()

	reRouter := &ReRouter{}

	_ = reRouter.RegisterFallbacks("localhost:1", []string{httpServer.URL})

	client := &http.Client{Transport: reRouter}

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://localhost:1/foo/bar", http.NoBody)

	res, _ := client.Do(req)

	_ = res.Body.Close()

	fmt.Println(res.StatusCode)

	// Output:
	// 200
}
