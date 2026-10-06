package wrapper

// ProbeArg is the hidden first argument that makes orchard check, from inside
// the namespaces it builds for the daemon, whether this host allows that. See
// ProbeSandbox.
const ProbeArg = "__probe-sandbox"

// CheckArg is the hidden first argument that runs the whole check from outside:
// orchard builds the sandbox, probes it, and exits 0 if the daemon can run here
// (or 1 with the reason on stderr). A host app uses it to explain a failure
// before the daemon is ever started.
const CheckArg = "__check-sandbox"
