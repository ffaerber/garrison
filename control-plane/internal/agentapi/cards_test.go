package agentapi

import (
	"slices"
	"testing"
)

func TestTheAnswerLinksTheCards(t *testing.T) {
	result := "Opened change lab/config#3. Nothing is changed yet.\nEnd your answer with these two lines…\nReview it: https://example.com/c/lab/config/3\nIn Gitea: https://git.example.com/lab/config/pulls/3"
	cards := reviewLine.FindAllString(result, -1)
	if !slices.Equal(cards, []string{"Review it: https://example.com/c/lab/config/3"}) {
		t.Fatalf("found %q", cards)
	}
	cards = append(cards, cards[0], "Review it: https://example.com/c/lab/example/4")
	got := missingCards("Done. Approve it here: https://example.com/c/lab/example/4", cards)
	if !slices.Equal(got, []string{"Review it: https://example.com/c/lab/config/3"}) {
		t.Errorf("missing: %q", got)
	}
	if got := missingCards("See https://example.com/c/lab/config/3 and https://example.com/c/lab/example/4", cards); len(got) != 0 {
		t.Errorf("both linked, yet %q", got)
	}
}

// A new provider's key goes on the Models page: the link is added when the
// answer only names the page.
func TestTheAnswerLinksTheModelsPage(t *testing.T) {
	result := "Opened change garrison/config#1, which adds the LLM provider smoke-chat.\nReview it: https://garrison.test/c/garrison/config/1\nIn Gitea: https://git.garrison.test/garrison/config/pulls/1\nSet its API key: https://garrison.test/models?provider=smoke-chat"
	cards := reviewLine.FindAllString(result, -1)
	if len(cards) != 2 {
		t.Fatalf("found %q", cards)
	}
	got := missingCards("Proposed. The API key is entered on the Models page. Review it: https://garrison.test/c/garrison/config/1", cards)
	if !slices.Equal(got, []string{"Set its API key: https://garrison.test/models?provider=smoke-chat"}) {
		t.Errorf("missing: %q", got)
	}
}
