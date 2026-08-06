package libraryreleaseverify

// Renderer/v1 is an external protocol contract. These values intentionally
// duplicate, rather than import, the independently implemented producer's
// limits. Any change requires a new renderer contract version and a real
// cross-producer acceptance vector.
const (
	rendererV1PlanSchema                    = 1
	rendererV1ArtifactSchema                = 1
	rendererV1MaxCompatibilityMetadataBytes = 64 << 10
	rendererV1MaxModuleGraphBytes           = 16 << 20
	rendererV1MaxGoSumBytes                 = 16 << 20
	rendererV1MaxSBOMBytes                  = 1 << 20
	rendererV1MaxSourceEntryBytes           = 128 << 20
	rendererV1MaxSourceExpandedBytes        = 256 << 20

	maxChecksumsBytes = 1 << 20
	maxArtifactBytes  = 512 << 20
	maxPublicKeyBytes = 64 << 10
)
