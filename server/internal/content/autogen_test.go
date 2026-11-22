package content

import (
	"strings"
	"testing"
)

func TestValidateDraftAgainstPlanRejectsShortAndMissingObserve(t *testing.T) {
	review := []PlanValidationTarget{{Character: "河", TargetRepetitions: 3}}
	observe := []PlanValidationTarget{{Character: "桥", TargetRepetitions: 2}}
	newCands := []PlanValidationTarget{{Character: "雪", TargetRepetitions: 3}}

	// 1. 3 句短稿且缺少观察字「桥」-> 必须判为不通过
	shortDraft := []string{
		"小熊在河边看到飘落的白雪。",
		"他顺着小河向前走，白雪真美。",
		"最后他回望小河和白雪笑了。",
	}
	resShort := ValidateDraftAgainstPlan(shortDraft, review, observe, newCands, DefaultMaxSentenceRunes)
	if resShort.Passed {
		t.Fatalf("expected 3-sentence draft missing observe target to fail validation")
	}
	if len(resShort.MissingObserveTargets) != 1 || resShort.MissingObserveTargets[0].Character != "桥" {
		t.Fatalf("expected missing observe target 「桥」, got %+v", resShort.MissingObserveTargets)
	}

	// 2. 16 句完整故事（>260 汉字）且覆盖所有复习/观察/新字 -> 通过校验
	fullDraft := make([]string, 0, 16)
	for i := 0; i < 16; i++ {
		fullDraft = append(fullDraft, "清晨小熊沿着清澈的小河走过坚固的木桥赏白雪。")
	}
	resFull := ValidateDraftAgainstPlan(fullDraft, review, observe, newCands, DefaultMaxSentenceRunes)
	if !resFull.Passed {
		t.Fatalf("expected valid 16-sentence story to pass validation, reasons=%s", strings.Join(resFull.FailureReasons, "; "))
	}
}

func TestValidateDraftRejectsSingleSentenceCramming(t *testing.T) {
	review := []PlanValidationTarget{{Character: "河", TargetRepetitions: 3}}
	observe := []PlanValidationTarget{{Character: "桥", TargetRepetitions: 2}}
	newCands := []PlanValidationTarget{{Character: "雪", TargetRepetitions: 3}}

	// 「河」只堆在第一句里出现 3 次，其余 15 句完全没有 -> 必须因未跨句分散而被拦截
	crammed := []string{
		"小熊在小河边看河水又望见一条大河，走过木桥赏白雪。",
	}
	for i := 0; i < 15; i++ {
		crammed = append(crammed, "清晨小熊沿着森林小路走过坚固木桥欣赏飘落白雪。")
	}
	res := ValidateDraftAgainstPlan(crammed, review, observe, newCands, DefaultMaxSentenceRunes)
	if res.Passed {
		t.Fatalf("expected single-sentence crammed target to fail dispersion check")
	}
	if len(res.MissingReviewTargets) != 1 || res.MissingReviewTargets[0].ActualSentences != 1 {
		t.Fatalf("expected 「河」 to fail dispersion with ActualSentences=1, got %+v", res.MissingReviewTargets)
	}
}

func TestValidateDraftRejectsNegativeTargetRepetitions(t *testing.T) {
	review := []PlanValidationTarget{{Character: "河", TargetRepetitions: -3}}
	fullDraft := make([]string, 0, 16)
	for i := 0; i < 16; i++ {
		fullDraft = append(fullDraft, "清晨小熊沿着森林小路走过坚固木桥欣赏飘落白雪。")
	}
	res := ValidateDraftAgainstPlan(fullDraft, review, nil, nil, DefaultMaxSentenceRunes)
	if res.Passed {
		t.Fatalf("expected negative target_repetitions to fail validation")
	}
	if len(res.MissingReviewTargets) != 1 {
		t.Fatalf("expected missing review target 「河」 under negative target_repetitions, got %+v", res.MissingReviewTargets)
	}
}
