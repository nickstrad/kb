# Developer shortcuts. macOS only; every target needs the fts5 tag (AGENTS.md).

.PHONY: test e2e e2e-update

test:
	go test -tags fts5 ./...
	go test -tags fts5,harnessfake -run 'TestHarnessClock|TestFakeModel' ./internal/cli/

# End-to-end journeys (docs/TESTING.md section 11): builds kb once with -tags fts5,harnessfake.
e2e:
	go test -tags fts5,e2e -count=1 ./internal/e2e/

# Rewrite the expected output of journeys in place. JOURNEY=name limits it to one. Review the diff.
e2e-update:
	go test -tags fts5,e2e -count=1 ./internal/e2e/ -run 'TestJourneys/$(if $(JOURNEY),^$(JOURNEY)$$)' -update
