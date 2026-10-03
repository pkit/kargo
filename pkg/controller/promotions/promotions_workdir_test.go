package promotions

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kargoapi "github.com/akuity/kargo/api/v1alpha1"
	"github.com/akuity/kargo/pkg/promotion"
)

// A container restart (e.g. an OOM kill) keeps the emptyDir mounted at the temp
// dir, so a Promotion's working directory outlives the process that was running
// its steps. That Promotion must not be resumed: the step may have left partial
// output, and the step may be what killed the process.
func Test_reconciler_promote_afterContainerRestart(t *testing.T) {
	scheme := k8sruntime.NewScheme()
	require.NoError(t, kargoapi.SchemeBuilder.AddToScheme(scheme))

	const testNamespace = "fake-namespace"

	testFreight := &kargoapi.Freight{
		ObjectMeta: metav1.ObjectMeta{Name: "fake-freight", Namespace: testNamespace},
		Origin: kargoapi.FreightOrigin{
			Kind: kargoapi.FreightOriginKindWarehouse,
			Name: "fake-warehouse",
		},
	}
	testStage := &kargoapi.Stage{
		ObjectMeta: metav1.ObjectMeta{Name: "fake-stage", Namespace: testNamespace},
		Spec: kargoapi.StageSpec{
			RequestedFreight: []kargoapi.FreightRequest{{
				Origin:  testFreight.Origin,
				Sources: kargoapi.FreightSources{Direct: true},
			}},
		},
	}
	// Interrupted while running its second step, whose partial output is still
	// in the working directory.
	testPromo := kargoapi.Promotion{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "fake-promotion",
			Namespace: testNamespace,
			UID:       types.UID("container-restart"),
		},
		Spec: kargoapi.PromotionSpec{
			Stage:   testStage.Name,
			Freight: testFreight.Name,
			Steps: []kargoapi.PromotionStep{
				{Uses: "fake-step", As: "first"},
				{Uses: "git-clone", As: "clone"},
			},
		},
		Status: kargoapi.PromotionStatus{
			Phase:       kargoapi.PromotionPhaseRunning,
			CurrentStep: 1,
			StepExecutionMetadata: kargoapi.StepExecutionMetadataList{
				{Alias: "first", Status: kargoapi.PromotionStepStatusSucceeded},
				{Alias: "clone", Status: kargoapi.PromotionStepStatusRunning},
			},
		},
	}

	newTestReconciler := func(promoted *bool) *reconciler {
		return &reconciler{
			kargoClient: fake.NewClientBuilder().WithScheme(scheme).WithObjects(
				testStage.DeepCopy(),
				testFreight.DeepCopy(),
			).Build(),
			promoEngine: &promotion.MockEngine{
				PromoteFn: func(
					_ context.Context,
					promoCtx promotion.Context,
					_ []promotion.Step,
				) (promotion.Result, error) {
					*promoted = true
					return promotion.Result{
						Status:      kargoapi.PromotionPhaseRunning,
						CurrentStep: promoCtx.StartFromStep,
					}, nil
				},
			},
		}
	}

	t.Run("same process resumes the current step in its own work dir", func(t *testing.T) {
		t.Setenv("TMPDIR", t.TempDir())
		var promoted bool
		r := newTestReconciler(&promoted)

		// First reconcile creates the work dir.
		_, _, err := r.promote(context.Background(), *testPromo.DeepCopy(), testStage, testFreight)
		require.NoError(t, err)
		require.True(t, promoted)

		promoted = false
		status, _, err := r.promote(context.Background(), *testPromo.DeepCopy(), testStage, testFreight)
		require.NoError(t, err)
		require.True(t, promoted)
		require.Equal(t, kargoapi.PromotionPhaseRunning, status.Phase)
	})

	t.Run("restarted container errors the promotion instead of resuming it", func(t *testing.T) {
		t.Setenv("TMPDIR", t.TempDir())
		checkout := filepath.Join(promotionWorkDir(testPromo.UID), "goval")
		require.NoError(t, os.MkdirAll(checkout, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(checkout, "file"), []byte("x"), 0o600))

		var promoted bool
		r := newTestReconciler(&promoted)

		status, requeue, err := r.promote(context.Background(), *testPromo.DeepCopy(), testStage, testFreight)
		require.NoError(t, err)
		require.Nil(t, requeue)
		require.False(t, promoted, "no step may run in the previous process's work dir")
		require.Equal(t, kargoapi.PromotionPhaseErrored, status.Phase)
		require.Contains(t, status.Message, `step "clone" was interrupted by a controller restart`)
		require.Equal(t, kargoapi.PromotionStepStatusSucceeded, status.StepExecutionMetadata[0].Status)
		require.Equal(t, kargoapi.PromotionStepStatusErrored, status.StepExecutionMetadata[1].Status)
		require.NotNil(t, status.StepExecutionMetadata[1].FinishedAt)
	})

	t.Run("replaced pod starts over in a fresh work dir", func(t *testing.T) {
		t.Setenv("TMPDIR", t.TempDir())
		var promoted bool
		r := newTestReconciler(&promoted)

		status, _, err := r.promote(context.Background(), *testPromo.DeepCopy(), testStage, testFreight)
		require.NoError(t, err)
		require.True(t, promoted)
		require.Equal(t, kargoapi.PromotionPhaseRunning, status.Phase)
		require.Equal(t, int64(0), status.CurrentStep)
	})
}
