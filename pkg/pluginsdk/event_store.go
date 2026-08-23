package pluginsdk

// EventStoreConfigKey is the reserved key by which a plugin tells the
// host, inside a member's own configuration document, that it holds the
// scheduled-event store for that member. The host reads this one field
// and nothing else: the rest of the document stays the plugin's business.
//
// It is a configuration key rather than an RPC because taking over is a
// per-member setting, not a property of the plugin: one calendar plugin
// serves members who connected an agenda and members who did not.
//
// Set it to true only once the member's backend is actually usable. From
// that moment their reminders stop being written to the host's own table,
// and a store that cannot answer means reminders that cannot be created —
// the host refuses out loud rather than silently scattering them across
// two backends.
const EventStoreConfigKey = "automata_event_store"
