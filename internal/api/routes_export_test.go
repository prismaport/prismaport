package api

// RegisteredRoutes is the route inventory for the route-coverage contract in the api_test
// package (route_coverage_integration_test.go): every pattern on the router of a server mounted
// like production.
func RegisteredRoutes() ([]string, error) {
	s, err := productionServer()
	if err != nil {
		return nil, err
	}
	return muxPatterns(s.mux), nil
}
