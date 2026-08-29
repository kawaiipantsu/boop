package tui

import (
	"fmt"
	"testing"
)

func TestComputeLayoutFillsExactlyTheScreen(t *testing.T) {
	// Every region budget must add up to the terminal height, at every size
	// worth caring about, or the frame tears on resize.
	for height := 1; height <= 60; height++ {
		for _, input := range []int{1, 3, 8, 40} {
			for _, approval := range []int{0, 1, 7, 30} {
				l := ComputeLayout(80, height, input, approval)
				if l.Rows() != height {
					t.Fatalf("height=%d input=%d approval=%d: rows=%d (%+v)",
						height, input, approval, l.Rows(), l)
				}
				if l.Body < 1 {
					t.Fatalf("height=%d input=%d approval=%d: body=%d", height, input, approval, l.Body)
				}
			}
		}
	}
}

func TestComputeLayoutShrinkOrder(t *testing.T) {
	tests := []struct {
		name                            string
		width, height, input, approval  int
		wantHeader, wantRules, wantBody int
		wantApproval, wantInput         int
		wantFooter                      int
	}{
		{
			name: "short screen keeps the footer", width: 40, height: 5, input: 1, approval: 0,
			wantHeader: 1, wantRules: 1, wantBody: 1, wantApproval: 0, wantInput: 1, wantFooter: 1,
		},
		{
			name: "roomy", width: 80, height: 24, input: 1, approval: 0,
			wantHeader: 1, wantRules: 2, wantBody: 19, wantApproval: 0, wantInput: 1, wantFooter: 1,
		},
		{
			name: "composer grows", width: 80, height: 24, input: 4, approval: 0,
			wantHeader: 1, wantRules: 2, wantBody: 16, wantApproval: 0, wantInput: 4, wantFooter: 1,
		},
		{
			name: "composer is capped", width: 80, height: 40, input: 99, approval: 0,
			wantHeader: 1, wantRules: 2, wantBody: 28, wantApproval: 0, wantInput: maxInputHeight, wantFooter: 1,
		},
		{
			name: "approval takes at most half", width: 80, height: 20, input: 1, approval: 30,
			wantHeader: 1, wantRules: 2, wantBody: 5, wantApproval: 10, wantInput: 1, wantFooter: 1,
		},
		{
			name: "tiny screen keeps header, transcript and composer", width: 40, height: 3, input: 3, approval: 0,
			wantHeader: 1, wantRules: 0, wantBody: 1, wantApproval: 0, wantInput: 1, wantFooter: 0,
		},
		{
			name: "one row is all transcript", width: 40, height: 1, input: 1, approval: 0,
			wantHeader: 0, wantRules: 0, wantBody: 1, wantApproval: 0, wantInput: 0, wantFooter: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l := ComputeLayout(tc.width, tc.height, tc.input, tc.approval)
			got := fmt.Sprintf("h=%d r=%d b=%d a=%d i=%d f=%d", l.Header, l.Rules, l.Body, l.Approval, l.Input, l.Footer)
			want := fmt.Sprintf("h=%d r=%d b=%d a=%d i=%d f=%d",
				tc.wantHeader, tc.wantRules, tc.wantBody, tc.wantApproval, tc.wantInput, tc.wantFooter)
			if got != want {
				t.Errorf("ComputeLayout(%d,%d,%d,%d) = %s, want %s",
					tc.width, tc.height, tc.input, tc.approval, got, want)
			}
		})
	}
}

func TestComputeLayoutClampsDegenerateSizes(t *testing.T) {
	l := ComputeLayout(0, 0, 0, 0)
	if l.Width != 1 || l.Height != 1 || l.Rows() != 1 {
		t.Fatalf("degenerate layout = %+v", l)
	}
	if l.ContentWidth() < 1 {
		t.Fatalf("content width = %d", l.ContentWidth())
	}
}

func TestHeaderSegments(t *testing.T) {
	tests := []struct{ width, left, right int }{
		{0, 0, 0},
		{3, 3, 0},
		{12, 4, 8},
		{80, 18, 62},
		{200, 18, 182},
	}
	for _, tc := range tests {
		left, right := HeaderSegments(tc.width)
		if left != tc.left || right != tc.right {
			t.Errorf("HeaderSegments(%d) = (%d, %d), want (%d, %d)", tc.width, left, right, tc.left, tc.right)
		}
		if tc.width > 0 && left+right != tc.width {
			t.Errorf("HeaderSegments(%d) does not cover the bar: %d+%d", tc.width, left, right)
		}
	}
}

func TestInputLinesClamps(t *testing.T) {
	for _, tc := range []struct{ in, want int }{{-4, 1}, {0, 1}, {1, 1}, {5, 5}, {99, maxInputHeight}} {
		if got := InputLines(tc.in); got != tc.want {
			t.Errorf("InputLines(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
