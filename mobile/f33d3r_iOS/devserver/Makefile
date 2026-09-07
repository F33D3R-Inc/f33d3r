# F33D3R iOS dev server

.PHONY: run reset build test vet

run:            ## start on 127.0.0.1:8081 with ./data
	go run .

reset:          ## wipe the database and reseed, then start
	go run . -reset

build:
	go build -o devserver .

test:
	go test ./...

vet:
	go vet ./...
