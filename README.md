# 📡 Go Reroute

Go ReRoute is a `http.RoundTripper` implementation that allows you to register alternative hosts for
an outgoing HTTP request.

## ⬇️ Installation

```bash
go get github.com/survivorbat/go-reroute
```

## 📋 Usage

```go
package main

func getClient() *http.Client {
	reRouter := &ReRouter{}

	_ = reRouter.RegisterFallbacks("localhost:1", []string{"localhost:8080"})

	client := &http.Client{Transport: reRouter}

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://localhost:1/foo/bar", http.NoBody)

	res, _ := client.Do(req)

  fmt.Println(res.Request.URL) // localhost:8080
}
```

## 🔭 Plans

- Perhaps add switchover capabilities
- Perhaps add loadbalancer capabilities
