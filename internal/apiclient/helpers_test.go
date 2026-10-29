package apiclient_test

import "net/http"

type bearerRT string

func (k bearerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+string(k))
	return http.DefaultTransport.RoundTrip(r)
}

func bearer(key string) *http.Client { return &http.Client{Transport: bearerRT(key)} }
