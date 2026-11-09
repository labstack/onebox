package main

// Helpers only tests call. They wrap or compose production code, and live
// here so the shipped binary carries no function nothing reaches.

func knownStatuses() []status {
	return []status{statusShipped, statusSchemaOnly, statusIntentOnly}
}
