test:
	docker compose -f docker-compose.test.yaml up toxiproxy integration_test --abort-on-container-exit --exit-code-from integration_test

.PHONY: schemas-verify schemas-verify-remote schemas-fetch

schemas-verify:
	scripts/fetch-schemas.sh --check

schemas-verify-remote:
	scripts/fetch-schemas.sh --verify-remote

schemas-fetch:
	scripts/fetch-schemas.sh --refresh
