package service

import "testing"

func TestDunningCoverageGapsAreExplicitAndNonMutating(t *testing.T) {
	gaps := dunningCoverageGaps([]DunningCoveragePolicy{
		{ID: "a", AgingFromDays: 1, AgingToDays: 30},
		{ID: "b", AgingFromDays: 61, AgingToDays: 90},
	})
	if len(gaps) != 2 || gaps[0].FromDays != 31 || gaps[0].ToDays != 60 || !gaps[1].OpenEnded || gaps[1].FromDays != 91 {
		t.Fatalf("gaps=%+v", gaps)
	}
	if gaps := dunningCoverageGaps([]DunningCoveragePolicy{{AgingFromDays: 5, AgingToDays: 90}}); len(gaps) != 2 || gaps[0].FromDays != 1 || gaps[0].ToDays != 4 {
		t.Fatalf("leading gap=%+v", gaps)
	}
	if gaps := dunningCoverageGaps(nil); len(gaps) != 1 || !gaps[0].OpenEnded || gaps[0].FromDays != 1 {
		t.Fatalf("empty coverage=%+v", gaps)
	}
	if gaps := dunningCoverageGaps([]DunningCoveragePolicy{{AgingFromDays: 1, AgingToDays: 30}, {AgingFromDays: 10, AgingToDays: 60}}); len(gaps) != 1 || !gaps[0].OpenEnded || gaps[0].FromDays != 61 {
		t.Fatalf("overlap coverage=%+v", gaps)
	}
}
