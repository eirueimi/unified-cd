# Contributing to unified-cd

Thanks for your interest in contributing. Bug reports, feature requests, and pull
requests are all welcome.

## License

unified-cd is licensed under the [Apache License, Version 2.0](LICENSE). By
submitting a contribution, you agree that it is provided under the same license
(Apache-2.0 §5, "inbound = outbound").

## Developer Certificate of Origin (DCO)

To keep the provenance of every contribution clear, this project uses the
[Developer Certificate of Origin](https://developercertificate.org/). It is a
lightweight, sign-off-based alternative to a CLA: you are not assigning
copyright, only certifying that you have the right to submit the code under the
project's license.

Every commit must carry a `Signed-off-by` line that matches the commit author,
which certifies the text below:

```
Developer Certificate of Origin
Version 1.1

By making a contribution to this project, I certify that:

(a) The contribution was created in whole or in part by me and I have the right
    to submit it under the open source license indicated in the file; or
(b) The contribution is based upon previous work that, to the best of my
    knowledge, is covered under an appropriate open source license and I have
    the right under that license to submit that work with modifications; or
(c) The contribution was provided directly to me by some other person who
    certified (a), (b) or (c) and I have not modified it.
(d) I understand and agree that this project and the contribution are public and
    that a record of the contribution (including all personal information I
    submit with it, including my sign-off) is maintained indefinitely and may be
    redistributed consistent with this project or the open source license(s)
    involved.
```

Add the sign-off automatically by committing with `-s`:

```bash
git commit -s -m "your message"
```

which appends, using your `git config user.name` / `user.email`:

```
Signed-off-by: Your Name <you@example.com>
```

If you forget it on the last commit, `git commit --amend -s --no-edit` fixes it;
for older commits use an interactive rebase.

## Development

See the "Development" section in the [README](README.md) for build and test
commands (`make build`, `make test`, `make dev-go`, `make dev-ui`, …). Please run
the tests before opening a pull request.
