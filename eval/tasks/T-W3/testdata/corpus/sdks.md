URL: https://docs.bindlewick.example/sdks
Title: SDKs and tools - Bindlewick Circulation API

# SDKs and tools

Bindlewick maintains SDKs for Java (`example.bindlewick:circulation`) and C#
(`Bindlewick.Circulation`), both on version 2 since their 3.0 releases. There is no official
Python SDK; the API is plain HTTPS and JSON.

Community libraries we know of:

- `pybindle` (Python), which covers version 1 only;
- `bindlewick-php` (PHP), on version 2.

The `bwcirc` command-line tool can call any endpoint: `bwcirc get /v2/patrons/p-1042/holds`.
