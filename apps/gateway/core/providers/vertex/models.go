package vertex

import (
	"strings"

	providerUtils "github.com/maximhq/bifrost/core/providers/utils"
	"github.com/maximhq/bifrost/core/schemas"
)

// VertexRankRequest represents the Discovery Engine rank API request.
type VertexRankRequest struct {
	Model                         *string            `json:"model,omitempty"`
	Query                         string             `json:"query"`
	Records                       []VertexRankRecord `json:"records"`
	TopN                          *int               `json:"topN,omitempty"`
	IgnoreRecordDetailsInResponse *bool              `json:"ignoreRecordDetailsInResponse,omitempty"`
	UserLabels                    map[string]string  `json:"userLabels,omitempty"`
}

// GetExtraParams implements providerUtils.RequestBodyWithExtraParams.
func (*VertexRankRequest) GetExtraParams() map[string]interface{} {
	return nil
}

const (
	vertexDefaultRankingConfigID   = "default_ranking_config"
	vertexDefaultRerankModel       = "semantic-ranker-default@latest"
	vertexMaxRerankRecordsPerQuery = 200
	vertexSyntheticRecordPrefix    = "idx:"
)

// VertexRankRecord represents a record for ranking.
type VertexRankRecord struct {
	ID      string  `json:"id"`
	Title   *string `json:"title,omitempty"`
	Content *string `json:"content,omitempty"`
}

// VertexRankResponse represents the Discovery Engine rank API response.
type VertexRankResponse struct {
	Records []VertexRankedRecord `json:"records"`
}

// VertexRankedRecord represents a ranked record in response.
type VertexRankedRecord struct {
	ID      string  `json:"id"`
	Score   float64 `json:"score"`
	Title   *string `json:"title,omitempty"`
	Content *string `json:"content,omitempty"`
}

type vertexRerankOptions struct {
	RankingConfig                 string
	IgnoreRecordDetailsInResponse bool
	UserLabels                    map[string]string
}

// ToBifrostListModelsResponse converts a Vertex AI publisher models response to Bifrost's format.
// This is for foundation models from the Model Garden (publishers.models.list endpoint).
func (response *VertexListPublisherModelsResponse) ToBifrostListModelsResponse(allowedModels schemas.WhiteList, blacklistedModels schemas.BlackList, aliases schemas.KeyAliases, unfiltered bool) *schemas.BifrostListModelsResponse {
	if response == nil {
		return nil
	}

	bifrostResponse := &schemas.BifrostListModelsResponse{
		Data: make([]schemas.Model, 0, len(response.PublisherModels)),
	}

	pipeline := &providerUtils.ListModelsPipeline{
		AllowedModels:     allowedModels,
		BlacklistedModels: blacklistedModels,
		Aliases:           aliases,
		Unfiltered:        unfiltered,
		ProviderKey:       schemas.Vertex,
		MatchFns:          providerUtils.DefaultMatchFns(),
	}
	if pipeline.ShouldEarlyExit() {
		return bifrostResponse
	}

	included := make(map[string]bool)

	for _, model := range response.PublisherModels {
		// Extract model ID from name (format: "publishers/google/models/gemini-1.5-pro")
		modelID := extractModelIDFromName(model.Name)
		if modelID == "" {
			continue
		}

		for _, result := range pipeline.FilterModel(modelID) {
			// Extract display name from supported actions if available
			displayName := result.ResolvedID
			if model.SupportedActions != nil && model.SupportedActions.Deploy != nil && model.SupportedActions.Deploy.ModelDisplayName != "" {
				displayName = model.SupportedActions.Deploy.ModelDisplayName
			}
			modelEntry := schemas.Model{
				ID:   string(schemas.Vertex) + "/" + result.ResolvedID,
				Name: schemas.Ptr(displayName),
			}
			if result.AliasValue != "" {
				modelEntry.Alias = schemas.Ptr(result.AliasValue)
			}
			bifrostResponse.Data = append(bifrostResponse.Data, modelEntry)
			included[strings.ToLower(result.ResolvedID)] = true
		}
	}

	bifrostResponse.Data = append(bifrostResponse.Data,
		pipeline.BackfillModels(included)...)

	bifrostResponse.NextPageToken = response.NextPageToken

	return bifrostResponse
}
