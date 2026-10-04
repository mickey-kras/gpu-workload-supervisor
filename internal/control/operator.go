package control

// OperatorPrecondition binds local owner-changing operations to an observed
// state and catalog. It is deliberately separate from automation preconditions.
type OperatorPrecondition struct {
	Incarnation           string
	Version               uint64
	Owner                 Owner
	ConfigurationRevision string
}
