package k8sagent

import (
	"bytes"
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStreamPodContainerLogs_CopiesStream(t *testing.T) {
	client := k8sfake.NewSimpleClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "ns"},
	})
	var out bytes.Buffer
	err := streamPodContainerLogs(context.Background(), client, "ns", "p1", "mysql", &out)
	require.NoError(t, err)
	assert.NotEmpty(t, out.String()) // fake returns a canned body
}
