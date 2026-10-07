module github.com/rksurwase/fedsearch

go 1.24.7

// Mirror used because the build sandbox could not reach golang.org; safe to keep or remove (then run go mod tidy).
replace golang.org/x/sys => github.com/golang/sys v0.28.0

require github.com/parquet-go/parquet-go v0.25.1

require (
	github.com/andybalholm/brotli v1.1.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/klauspost/compress v1.17.9 // indirect
	github.com/pierrec/lz4/v4 v4.1.21 // indirect
	golang.org/x/sys v0.21.0 // indirect
)
