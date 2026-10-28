# Notes

Two suggestions from Claude Code that I rejected while working on this task.

1. Extending the central `board.Store` interface. Claude first proposed adding `HasRunningJob` and `ListRunningJobs` to `board.Store` for the 409 rule and for the worker. That would have touched `board.go` and the in-memory test store next to the protected tests. I rejected it: the handler checks the 409 rule with the `ListJobs` method it already has, and the worker declares its own two-method interface, `worker.Store`, which `*store.Store` satisfies implicitly. I checked the result with `git diff`: the `Store` interface and the `mem` test store are unchanged, and `go test ./...` is green.

2. Keeping `updatedAt` in the sheet JSON. Claude suggested leaving the field in because it is harmless, but the README example does not list it, and the README is the spec. I hid it with `json:"-"` so a sheet has exactly the documented fields. I checked it with `curl` against the running stack and with the test `TestSheetJSONHasExactlyTheDocumentedFields`.
