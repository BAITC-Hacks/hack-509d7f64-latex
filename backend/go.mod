module voice-router

go 1.25

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/pgx/v5 v5.7.6 // indirect
	golang.org/x/crypto v0.37.0 // indirect
	golang.org/x/text v0.24.0 // indirect
)

// The catalog/knowledge base is embedded from ../voice_router_dataset, which is
// shared with the Python services, so it is wired in as a local module.
require voice-router/voice_router_dataset v0.0.0

replace voice-router/voice_router_dataset => ../voice_router_dataset
