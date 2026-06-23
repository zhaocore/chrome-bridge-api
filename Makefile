APP := chrome-bridge
CMD := ./cmd/chrome-bridge
LDFLAGS :=

ifneq ($(strip $(PORT)),)
LDFLAGS += -X main.defaultPort=$(PORT)
endif

.PHONY: build test clean

build:
	env -u GOROOT go build $(if $(LDFLAGS),-ldflags "$(LDFLAGS)") -o ./bin/$(APP) $(CMD)

test:
	env -u GOROOT GOTOOLCHAIN=local go test ./...

clean:
	rm -f $(APP)
