.PHONY: validate

validate:
	@files="$$(gofmt -l .)"; \
	if [ -n "$$files" ]; then \
		echo "gofmt found issues in:"; \
		echo "$$files"; \
		gofmt -w $$files; \
		echo "formatted the above files"; \
	else \
		echo "gofmt: no issues found"; \
	fi