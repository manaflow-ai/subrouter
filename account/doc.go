// Package account holds the account value types shared by everything that
// routes traffic: the CLI, the proxy, the scheduler, and out-of-tree consumers
// such as the hosted gateway.
//
// It is deliberately types-only and depends on nothing but the standard
// library. Persistence lives elsewhere, because the two consumers store
// accounts in incompatible ways: the CLI keeps them in files under
// ~/.subrouter, while a hosted deployment keeps them in a database with the
// credentials encrypted. Keeping this package free of a storage dependency is
// what lets both share the scheduler.
package account
