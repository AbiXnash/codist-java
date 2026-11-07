# codist-java

The Java engine of [Codist](https://github.com/AbiXnash/codist): it reads Java source with a pure-Go tree-sitter
parser, so no C compiler is needed, and finds:

- **types and Spring facts**: classes, methods, fields, stereotypes (controller, service, repository, Feign client),
  REST routes, modules with their `spring.application.name` and port;
- **code checks**: 15 exact rules such as an empty catch, `==` on strings, a shared `SimpleDateFormat`, or a
  repository call inside a loop, each limited to changed lines when asked;
- **the class graph and call graph**: extends, implements, injects, uses, and Feign calls from a client to the
  controller that serves its route; method calls with confirmed or name-only receivers;
- **cost hints**: loop depth, calls inside loops, and branching per method.

```go
import java "github.com/AbiXnash/codist-java"

ix := java.Build(sources, buildFiles) // path -> content
calls := java.BuildCallGraph(ix.Sources, ix.Types).Calls()
issues := java.Check(java.Parse(path, src), changedLines)
```

Build and test with the Java-only grammar, which keeps binaries small:

```bash
go test -tags 'grammar_subset grammar_subset_java' ./...
```
