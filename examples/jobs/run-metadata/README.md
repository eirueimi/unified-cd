# Run metadata example

`job.yaml` shows how a step reads run metadata from its environment:

- the `announce` step prints a link to its own run with `$UNIFIED_RUN_URL` (the link is only
  available when the controller knows its public address, see `controller.externalUrl` in the Helm
  chart or `UNIFIED_EXTERNAL_URL`);
- the `finally:` step `report` prints `$UNIFIED_RUN_STATUS`, `$UNIFIED_RUN_DURATION_SECONDS` and
  `$UNIFIED_RUN_FAILURES_JSON`, which exist only in `finally:`.

```bash
unified-cd apply -f examples/jobs/run-metadata/job.yaml
unified-cd run trigger run-metadata-demo                      # status: Succeeded, failures: []
unified-cd run trigger run-metadata-demo --param fail=true    # status: Failed, failures lists "work"
```

Replace the `echo` lines with a call to your chat, PR-comment or deploy-record tool to post the
link. The full list of variables and template fields is in
[Run Metadata](../../../docs/user-guide/writing-jobs/run-metadata.md).
