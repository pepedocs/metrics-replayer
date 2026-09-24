.PHONY: test test-unit test-e2e clean

test: test-unit test-e2e

test-unit:
	go test -v ./...

test-e2e:
	docker compose up --build -d
	@echo "waiting for services to be ready..."
	@sleep 5
	PROMETHEUS_URL=http://localhost:9091 go test -v -count=1 ./e2e/
	docker compose down -v

clean:
	docker compose down -v
