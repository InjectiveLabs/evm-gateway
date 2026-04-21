package kvcapnp

//go:generate sh -c "capnp compile -I \"$(go list -m -f '{{.Dir}}' capnproto.org/go/capnp/v3)/std\" -ogo kv.capnp"
