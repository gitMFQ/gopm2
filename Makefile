.PHONY: build test test-integration vet fmt run clean demo install uninstall

BIN_DIR := bin
GOPM2 := $(BIN_DIR)/gopm2
GOPM2D := $(BIN_DIR)/gopm2d
DEMO := $(BIN_DIR)/demo

build: $(GOPM2) $(GOPM2D) $(DEMO)

$(GOPM2):
	mkdir -p $(BIN_DIR)
	go build -o $@ ./cmd/gopm2

$(GOPM2D):
	mkdir -p $(BIN_DIR)
	go build -o $@ ./cmd/gopm2d

$(DEMO):
	mkdir -p $(BIN_DIR)
	go build -o $@ ./examples/demo

fmt:
	gofmt -w .

vet:
	go vet ./...

test:
	go test -timeout 120s ./...

test-integration: build
	GOPM2_BIN=$(CURDIR)/$(GOPM2) GOPM2D_BIN=$(CURDIR)/$(GOPM2D) go test -tags=integration -timeout 300s ./test/...

lint: fmt vet test

run: build
	./$(GOPM2D) --data-dir ~/.gopm2

clean:
	rm -rf $(BIN_DIR)

install: build
	install -d $(DESTDIR)/usr/local/bin
	install -m 0755 $(GOPM2) $(DESTDIR)/usr/local/bin/
	install -m 0755 $(GOPM2D) $(DESTDIR)/usr/local/bin/

uninstall:
	rm -f $(DESTDIR)/usr/local/bin/gopm2 $(DESTDIR)/usr/local/bin/gopm2d

.PHONY: all
all: build
