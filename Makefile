.PHONY: test test-unit test-e2e up down logs clean

test: test-unit test-e2e

test-unit:
	go test -v $$(go list ./... | grep -v /tests/e2e)

test-e2e:
	docker compose up --build -d
	@echo "waiting for services to be ready..."
	@sleep 5
	PROMETHEUS_URL=http://localhost:9091 go test -v -count=1 ./tests/e2e/
	docker compose down -v

up:
	docker compose up --build -d

down:
	docker compose down

logs:
	docker compose logs -f

clean:
	docker compose down -v
