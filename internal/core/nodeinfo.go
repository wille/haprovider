package core

// NodeInfo is the chain-agnostic node information a healthcheck collects from a
// provider.
type NodeInfo struct {
	ClientVersion string
	ChainID       string
	BlockHeight   uint64
}
