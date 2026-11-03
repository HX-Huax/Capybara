package model

import (
	"encoding/json"
	"testing"
	"time"
)

// 同优先级按提交时间排序，时间相同再按 ID 排序，避免重启改变队列顺序。
func TestQueueSortAndLegacyDefault(t *testing.T) {
	now := time.Now()
	jobs := []Job{{ID: 4, Priority: -1, SubmitAt: now}, {ID: 3, Priority: 10, SubmitAt: now}, {ID: 2, Priority: 10, SubmitAt: now}, {ID: 1, Priority: 10, SubmitAt: now.Add(-time.Second)}}
	SortQueue(jobs)
	for i, id := range []int64{1, 2, 3, 4} {
		if jobs[i].ID != id {
			t.Fatalf("排序错误: %+v", jobs)
		}
	}
	var old Job
	if err := json.Unmarshal([]byte(`{"id":1,"state":"pending"}`), &old); err != nil {
		t.Fatal(err)
	}
	if old.Priority != 0 {
		t.Fatal("旧记录应使用默认优先级 0")
	}
}
