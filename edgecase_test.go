package goorderfulfillment

import (
	"errors"
	"math/rand"
	"strconv"
	"sync"
	"testing"
)

// ---------- 本轮补充边界用例 ----------

// 同一仓库报短缺后补货，缺口的新分配必须以“新版本、新分配 ID”的形式
// 从同一仓重现；旧分配的迟到回执/发货不能落到新分配上，也不能重复发货。
func TestShortage_RestockSameWarehouseIsNewVersionedAllocation(t *testing.T) {
	svc := newTestService(t)
	mustRegister(t, svc, "W1", "A", 7)
	mustRegister(t, svc, "W2", "A", 3)
	conf := mustConfirm(t, svc, "O1", []OrderLine{{SKU: "A", Quantity: 10}})
	w1 := allocByWh(conf.Allocations, "W1")
	w2 := allocByWh(conf.Allocations, "W2")

	// W1 拣齐 7；W2 全短缺 3，此时无后备库存，自动重配失败。
	res, err := svc.PickingReceipt(PickingReceiptRequest{Items: []PickingReceiptItem{
		{AllocationID: w1.ID, PickedQty: 7},
		{AllocationID: w2.ID, ShortageQty: 3},
	}})
	if err != nil || !errors.Is(res.ReallocateError, ErrInsufficientInventory) {
		t.Fatalf("receipt: err=%v reerr=%v", err, res.ReallocateError)
	}

	// W2 补货 3 件后重配：同一仓库以全新 v2 分配承接。
	mustRegister(t, svc, "W2", "A", 3)
	re, err := svc.Reallocate(ReallocateRequest{OrderID: "O1"})
	if err != nil {
		t.Fatalf("manual reallocate: %v", err)
	}
	if re.Version != 2 || len(re.Allocations) != 1 || re.Allocations[0].WarehouseID != "W2" {
		t.Fatalf("bad v2 plan: %+v", re.Allocations)
	}
	newW2 := re.Allocations[0]
	if newW2.ID == w2.ID {
		t.Fatalf("new allocation must be versioned distinctly: old=%s new=%s", w2.ID, newW2.ID)
	}

	// 旧分配的迟到成功回执必须拒绝；旧分配无货可发也不能发。
	if _, err := svc.ReceiptPick(w2.ID, 3); !errors.Is(err, ErrStaleAllocation) {
		t.Fatalf("stale receipt on old alloc: want ErrStaleAllocation, got %v", err)
	}
	if _, err := svc.Ship(ShipmentRequest{AllocationID: w2.ID, Quantity: 1}); !errors.Is(err, ErrShipExceedsPicked) {
		t.Fatalf("ship old alloc: want ErrShipExceedsPicked, got %v", err)
	}

	// 只有新分配可以拣发。
	if _, err := svc.ReceiptPick(newW2.ID, 3); err != nil {
		t.Fatalf("pick new alloc: %v", err)
	}
	if _, err := svc.Ship(ShipmentRequest{AllocationID: newW2.ID, Quantity: 3}); err != nil {
		t.Fatalf("ship new alloc: %v", err)
	}
	if _, err := svc.Ship(ShipmentRequest{AllocationID: w1.ID, Quantity: 7}); err != nil {
		t.Fatalf("ship W1: %v", err)
	}

	detail, _ := svc.GetFulfillmentDetail("O1")
	if detail.Status != StatusShipped {
		t.Fatalf("status = %s, want shipped", detail.Status)
	}
	assertNoDoubleShip(t, detail)
	assertInventoryInvariant(t, svc)
	// W2 账面 6 件：3 件核销损耗、3 件实际发出。
	inv := svc.GetInventory("W2", "A")
	if inv.OnHand != 6 || inv.Shipped != 3 || inv.Lost != 3 || inv.Available != 0 {
		t.Fatalf("W2 ledger = %+v", inv)
	}
}

// 一批回执跨多个订单、且各自触发重配：每个订单的成功/失败都必须可见，
// 不能出现“一个订单成功、另一个失败”时失败结果被覆盖。
func TestPickingReceipt_CrossOrderReallocationResults(t *testing.T) {
	svc := newTestService(t)
	// 订单 O-OK：独占 W1(2)；W2 在确认之后到货 3 件作为后备，短缺后自动重配成功。
	mustRegister(t, svc, "W1", "A", 2)
	confOK := mustConfirm(t, svc, "O-OK", []OrderLine{{SKU: "A", Quantity: 2}})
	mustRegister(t, svc, "W2", "A", 3)
	// 订单 O-BAD：独占 W3 且全局再无后备，短缺后自动重配失败。
	mustRegister(t, svc, "W3", "A", 4)
	confBad := mustConfirm(t, svc, "O-BAD", []OrderLine{{SKU: "A", Quantity: 4}})

	allocOK := allocByWh(confOK.Allocations, "W1")
	allocBad := allocByWh(confBad.Allocations, "W3")
	if allocOK == nil || allocBad == nil {
		t.Fatalf("unexpected v1 plans: ok=%+v bad=%+v", confOK.Allocations, confBad.Allocations)
	}

	res, err := svc.PickingReceipt(PickingReceiptRequest{Items: []PickingReceiptItem{
		{AllocationID: allocOK.ID, ShortageQty: 2},
		{AllocationID: allocBad.ID, ShortageQty: 4},
	}})
	if err != nil {
		t.Fatalf("receipt: %v", err)
	}

	if len(res.Reallocations) != 2 {
		t.Fatalf("want per-order results for 2 orders, got %+v", res.Reallocations)
	}
	results := map[string]OrderReallocation{}
	for _, r := range res.Reallocations {
		results[r.OrderID] = r
	}
	badRes := results["O-BAD"]
	okRes := results["O-OK"]
	if !errors.Is(badRes.Err, ErrInsufficientInventory) || badRes.Version != 0 || badRes.Allocations != nil {
		t.Errorf("O-BAD reallocation should fail: %+v", badRes)
	}
	if okRes.Err != nil || okRes.Version != 2 || sumQty(okRes.Allocations) != 2 {
		t.Errorf("O-OK reallocation should succeed with v2: %+v", okRes)
	}
	if a := allocByWh(okRes.Allocations, "W2"); a == nil {
		t.Errorf("O-OK v2 should reallocate from backup W2: %+v", okRes.Allocations)
	}

	// 失败订单的状态与缺口保留：补货后可手动重配。
	detail, _ := svc.GetFulfillmentDetail("O-BAD")
	if detail.LineSummary[0].Outstanding != 4 {
		t.Errorf("O-BAD outstanding = %d, want 4", detail.LineSummary[0].Outstanding)
	}
	if detail.Status != StatusInFulfillment {
		t.Errorf("O-BAD status = %s, want in_fulfillment", detail.Status)
	}
	mustRegister(t, svc, "W4", "A", 4)
	re, err := svc.Reallocate(ReallocateRequest{OrderID: "O-BAD"})
	if err != nil || re.Version != 2 || sumQty(re.Allocations) != 4 {
		t.Fatalf("manual reallocate after restock: ver=%v err=%v", re, err)
	}
	assertInventoryInvariant(t, svc)
}

// 一批回执同时报告同一订单两个 SKU 的短缺：只生成一个新版本，
// 且该版本一次覆盖两个 SKU 的缺口。
func TestPickingReceipt_MultiSKUShortageOneVersion(t *testing.T) {
	svc := newTestService(t)
	mustRegister(t, svc, "W1", "A", 7)
	mustRegister(t, svc, "W2", "A", 3)
	mustRegister(t, svc, "W1", "B", 6)
	mustRegister(t, svc, "W2", "B", 4)
	mustRegister(t, svc, "W3", "A", 3)
	mustRegister(t, svc, "W3", "B", 4)
	conf := mustConfirm(t, svc, "O1", []OrderLine{{SKU: "A", Quantity: 10}, {SKU: "B", Quantity: 10}})
	var w2a, w2b *Allocation
	for _, a := range conf.Allocations {
		switch {
		case a.WarehouseID == "W2" && a.LineSKU == "A":
			w2a = a
		case a.WarehouseID == "W2" && a.LineSKU == "B":
			w2b = a
		}
	}
	if w2a == nil || w2b == nil {
		t.Fatalf("v1 plan missing W2 allocations: %+v", conf.Allocations)
	}

	res, err := svc.PickingReceipt(PickingReceiptRequest{Items: []PickingReceiptItem{
		{AllocationID: w2a.ID, ShortageQty: 3},
		{AllocationID: w2b.ID, ShortageQty: 4},
	}})
	if err != nil {
		t.Fatalf("receipt: %v", err)
	}
	if res.ReallocatedVersion != 2 || len(res.Reallocated) != 2 || sumQty(res.Reallocated) != 7 {
		t.Fatalf("want single v2 covering A:3+B:4, got v%d %+v err=%v",
			res.ReallocatedVersion, res.Reallocated, res.ReallocateError)
	}
	detail, _ := svc.GetFulfillmentDetail("O1")
	if len(detail.Versions) != 2 {
		t.Fatalf("versions = %d, want 2", len(detail.Versions))
	}
	covered := map[string]int{}
	for _, a := range res.Reallocated {
		if a.WarehouseID != "W3" {
			t.Errorf("v2 alloc should use W3 stock: %+v", a)
		}
		covered[a.LineSKU] += a.Quantity
	}
	if covered["A"] != 3 || covered["B"] != 4 {
		t.Errorf("v2 gap coverage = %+v", covered)
	}
	assertInventoryInvariant(t, svc)
}

// 取消后再以同号同内容确认仍是幂等返回原方案，不会重复占用库存或重建订单。
func TestConfirmOrder_IdempotentAfterCancel(t *testing.T) {
	svc := newTestService(t)
	mustRegister(t, svc, "W1", "A", 5)
	mustConfirm(t, svc, "O1", []OrderLine{{SKU: "A", Quantity: 5}})
	if _, err := svc.Cancel(CancelRequest{OrderID: "O1"}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if inv := svc.GetInventory("W1", "A"); inv.Available != 5 || inv.Reserved != 0 {
		t.Fatalf("after cancel: %+v", inv)
	}
	re, err := svc.ConfirmOrder(ConfirmOrderRequest{ExternalOrderID: "O1", Lines: []OrderLine{{SKU: "A", Quantity: 5}}})
	if err != nil || !re.Reconfirmed || re.Status != StatusCancelled {
		t.Fatalf("reconfirm after cancel: %+v err=%v", re, err)
	}
	if inv := svc.GetInventory("W1", "A"); inv.Available != 5 || inv.Reserved != 0 {
		t.Errorf("idempotent reconfirm must not re-hold stock: %+v", inv)
	}
	detail, _ := svc.GetFulfillmentDetail("O1")
	if len(detail.Versions) != 1 {
		t.Errorf("versions = %d, still want 1", len(detail.Versions))
	}
}

// TestRandomLedgerModel 是随机化模型测试：以随机序列交错执行
// 确认/拣货(成功或短缺)/手动重配/发货/取消/补货，并在多个检查点
// 按 SKU 审计全局账实恒等式与“分配账 == 仓库账”，用于捕捉并发与重配
// 交错下的超卖、重复发货与账实漂移。
func TestRandomLedgerModel(t *testing.T) {
	svc := newTestService(t)
	rng := rand.New(rand.NewSource(20260930))
	whs := []string{"W1", "W2", "W3", "W4"}
	skus := []string{"A", "B", "C"}

	for _, w := range whs {
		for _, s := range skus {
			mustRegister(t, svc, w, s, 1+rng.Intn(20))
		}
	}

	orderSeq := 0
	var activeIDs []string

	type cols struct{ avail, res, picked, shipped, lost, onhand int }
	audit := func(stage string) {
		t.Helper()
		book := map[string]cols{}
		svc.mu.Lock()
		for _, inv := range svc.inventories {
			b := book[inv.SKU]
			b.avail += inv.Available
			b.res += inv.Reserved
			b.picked += inv.Picked
			b.shipped += inv.Shipped
			b.lost += inv.Lost
			b.onhand += inv.OnHand
			book[inv.SKU] = b
		}
		calc := map[string]cols{}
		for _, order := range svc.orders {
			for _, v := range orderVersions(order) {
				for _, a := range v.Allocations {
					c := calc[a.LineSKU]
					if a.Status == AllocActive {
						c.res += a.outstanding()
					}
					c.picked += a.PickedQty - a.ShippedQty
					c.shipped += a.ShippedQty
					c.lost += a.ShortageQty
					calc[a.LineSKU] = c
				}
			}
		}
		svc.mu.Unlock()
		for _, s := range skus {
			b := book[s]
			if b.avail+b.res+b.picked+b.shipped+b.lost != b.onhand {
				t.Fatalf("[%s] sku %s identity broken: %+v", stage, s, b)
			}
			c := calc[s]
			if c.res != b.res || c.picked != b.picked || c.shipped != b.shipped || c.lost != b.lost {
				t.Fatalf("[%s] sku %s alloc/warehouse mismatch book=%+v alloc=%+v", stage, s, b, c)
			}
		}
	}

	const steps = 4000
	for step := 0; step < steps; step++ {
		switch rng.Intn(7) {
		case 0, 1: // 确认订单
			var lines []OrderLine
			for _, s := range skus {
				if rng.Intn(2) == 0 {
					lines = append(lines, OrderLine{SKU: s, Quantity: 1 + rng.Intn(6)})
				}
			}
			if len(lines) == 0 {
				continue
			}
			id := "O" + strconv.Itoa(orderSeq)
			orderSeq++
			if _, err := svc.ConfirmOrder(ConfirmOrderRequest{ExternalOrderID: id, Lines: lines}); err == nil {
				activeIDs = append(activeIDs, id)
			}
		case 2: // 拣货回执（成功或短缺）
			if len(activeIDs) == 0 {
				continue
			}
			d, err := svc.GetFulfillmentDetail(activeIDs[rng.Intn(len(activeIDs))])
			if err != nil || d.ActiveVersion == 0 {
				continue
			}
			v := d.Versions[d.ActiveVersion-1]
			var items []PickingReceiptItem
			for _, a := range v.Allocations {
				if a.Status != AllocActive {
					continue
				}
				rem := a.Quantity - a.PickedQty - a.ShortageQty
				if rem <= 0 {
					continue
				}
				if rng.Intn(2) == 0 {
					items = append(items, PickingReceiptItem{AllocationID: a.ID, PickedQty: 1 + rng.Intn(rem)})
				} else {
					items = append(items, PickingReceiptItem{AllocationID: a.ID, ShortageQty: 1 + rng.Intn(rem)})
				}
				break
			}
			if len(items) > 0 {
				if _, err := svc.PickingReceipt(PickingReceiptRequest{Items: items}); err != nil {
					t.Fatalf("step %d unexpected receipt err: %v items=%+v", step, err, items)
				}
			}
		case 3: // 手动重配
			if len(activeIDs) == 0 {
				continue
			}
			_, _ = svc.Reallocate(ReallocateRequest{OrderID: activeIDs[rng.Intn(len(activeIDs))]})
		case 4: // 发货
			if len(activeIDs) == 0 {
				continue
			}
			d, _ := svc.GetFulfillmentDetail(activeIDs[rng.Intn(len(activeIDs))])
			for _, v := range d.Versions {
				for _, a := range v.Allocations {
					rem := a.PickedQty - a.ShippedQty
					if rem <= 0 {
						continue
					}
					q := rem
					if rng.Intn(2) == 0 {
						q = 1 + rng.Intn(rem)
					}
					if _, err := svc.Ship(ShipmentRequest{AllocationID: a.ID, Quantity: q}); err != nil {
						t.Fatalf("step %d unexpected ship err: %v", step, err)
					}
					break
				}
			}
		case 5: // 取消
			if len(activeIDs) == 0 {
				continue
			}
			idx := rng.Intn(len(activeIDs))
			id := activeIDs[idx]
			activeIDs = append(activeIDs[:idx], activeIDs[idx+1:]...)
			_, _ = svc.Cancel(CancelRequest{OrderID: id})
		case 6: // 随机补货
			if _, err := svc.RegisterInventory(RegisterInventoryRequest{
				WarehouseID: whs[rng.Intn(len(whs))],
				SKU:         skus[rng.Intn(len(skus))],
				Quantity:    1 + rng.Intn(5),
			}); err != nil {
				t.Fatal(err)
			}
		}
		if step%50 == 0 {
			audit("step" + strconv.Itoa(step))
		}
	}
	audit("sequential-final")

	for _, id := range activeIDs {
		d, err := svc.GetFulfillmentDetail(id)
		if err != nil {
			t.Fatal(err)
		}
		assertNoDoubleShip(t, d)
	}

	// 并发冒烟：查询/取消/重配被并发调用，-race 下不应出现数据竞争或崩溃。
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			for k := 0; k < 200; k++ {
				svc.GetInventory(whs[i%len(whs)], skus[k%len(skus)])
				if len(activeIDs) > 0 {
					id := activeIDs[k%len(activeIDs)]
					_, _ = svc.GetFulfillmentDetail(id)
					_, _ = svc.Cancel(CancelRequest{OrderID: id})
					_, _ = svc.Reallocate(ReallocateRequest{OrderID: id})
				}
			}
		}(i)
	}
	close(start)
	wg.Wait()
	audit("concurrent-final")
}
