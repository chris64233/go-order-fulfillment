package goorderfulfillment

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

// ---------- 测试辅助 ----------

func newTestService(t *testing.T) *FulfillmentService {
	t.Helper()
	return NewFulfillmentService()
}

func mustRegister(t *testing.T, svc *FulfillmentService, wh, sku string, qty int) {
	t.Helper()
	if _, err := svc.RegisterInventory(RegisterInventoryRequest{WarehouseID: wh, SKU: sku, Quantity: qty}); err != nil {
		t.Fatalf("register inventory %s/%s=%d: %v", wh, sku, qty, err)
	}
}

func mustConfirm(t *testing.T, svc *FulfillmentService, orderID string, lines []OrderLine) *Confirmation {
	t.Helper()
	c, err := svc.ConfirmOrder(ConfirmOrderRequest{ExternalOrderID: orderID, Lines: lines})
	if err != nil {
		t.Fatalf("confirm order %s: %v", orderID, err)
	}
	return c
}

// assertInventoryInvariant 校验库存恒等式 OnHand == Available+Reserved+Picked+Shipped+Lost。
func assertInventoryInvariant(t *testing.T, svc *FulfillmentService) {
	t.Helper()
	for _, inv := range svc.inventories {
		sum := inv.Available + inv.Reserved + inv.Picked + inv.Shipped + inv.Lost
		if sum != inv.OnHand {
			t.Errorf("inventory %s/%s invariant broken: onhand=%d but avail=%d reserved=%d picked=%d shipped=%d lost=%d (sum=%d)",
				inv.WarehouseID, inv.SKU, inv.OnHand, inv.Available, inv.Reserved, inv.Picked, inv.Shipped, inv.Lost, sum)
		}
	}
}

// assertNoDoubleShip 校验每个 SKU 的全仓发货总量不超过订单需求量，
// 且单条分配发货量不超过其拣出量（同一件商品不可能被两个仓重复发出）。
func assertNoDoubleShip(t *testing.T, detail *FulfillmentDetail) {
	t.Helper()
	demand := map[string]int{}
	for _, l := range detail.Lines {
		demand[l.SKU] = l.Quantity
	}
	shippedBySKU := map[string]int{}
	pickedByAlloc := map[string]int{}
	for _, v := range detail.Versions {
		for _, a := range v.Allocations {
			pickedByAlloc[a.ID] = a.PickedQty
			if a.ShippedQty > a.PickedQty {
				t.Errorf("allocation %s shipped %d > picked %d", a.ID, a.ShippedQty, a.PickedQty)
			}
			shippedBySKU[a.LineSKU] += a.ShippedQty
		}
	}
	for sku, q := range shippedBySKU {
		if q > demand[sku] {
			t.Errorf("sku %s total shipped %d exceeds demand %d (double shipment)", sku, q, demand[sku])
		}
	}
	for _, sh := range detail.Shipments {
		if sh.Quantity <= 0 {
			t.Errorf("shipment %s non-positive quantity %d", sh.ID, sh.Quantity)
		}
	}
}

func allocByWh(allocs []*Allocation, wh string) *Allocation {
	for _, a := range allocs {
		if a.WarehouseID == wh {
			return a
		}
	}
	return nil
}

// ---------- 1. 跨仓拆分与原子性 ----------

func TestConfirmOrder_SplitAcrossWarehouses(t *testing.T) {
	svc := newTestService(t)
	mustRegister(t, svc, "W1", "A", 3)
	mustRegister(t, svc, "W2", "A", 8)
	mustRegister(t, svc, "W1", "B", 5)

	conf := mustConfirm(t, svc, "O1", []OrderLine{{SKU: "A", Quantity: 10}, {SKU: "B", Quantity: 5}})
	if conf.Version != 1 || conf.Reconfirmed {
		t.Fatalf("unexpected confirmation: %+v", conf)
	}

	// A=10 跨两仓：W2 可用 8 优先，W1 补 2；B=5 全部由 W1 承担。
	var aTotal int
	var sawW1A, sawW2A bool
	for _, a := range conf.Allocations {
		aTotal += a.Quantity
		switch {
		case a.LineSKU == "A" && a.WarehouseID == "W1":
			sawW1A = true
			if a.Quantity != 2 {
				t.Errorf("W1/A allocation = %d, want 2", a.Quantity)
			}
		case a.LineSKU == "A" && a.WarehouseID == "W2":
			sawW2A = true
			if a.Quantity != 8 {
				t.Errorf("W2/A allocation = %d, want 8", a.Quantity)
			}
		case a.LineSKU == "B" && a.WarehouseID == "W1":
			if a.Quantity != 5 {
				t.Errorf("W1/B allocation = %d, want 5", a.Quantity)
			}
		default:
			t.Errorf("unexpected allocation %+v", a)
		}
	}
	if aTotal != 15 || !sawW1A || !sawW2A {
		t.Fatalf("unexpected allocations: %+v", conf.Allocations)
	}

	// 库存已按方案占用。
	w1a := svc.GetInventory("W1", "A")
	if w1a.Available != 1 || w1a.Reserved != 2 {
		t.Errorf("W1/A = %+v, want available=1 reserved=2", w1a)
	}
	w2a := svc.GetInventory("W2", "A")
	if w2a.Available != 0 || w2a.Reserved != 8 {
		t.Errorf("W2/A = %+v, want available=0 reserved=8", w2a)
	}
	assertInventoryInvariant(t, svc)
}

func TestConfirmOrder_RejectAllIfAnyLineShort(t *testing.T) {
	svc := newTestService(t)
	mustRegister(t, svc, "W1", "A", 100)
	mustRegister(t, svc, "W1", "B", 2) // B 不足

	_, err := svc.ConfirmOrder(ConfirmOrderRequest{
		ExternalOrderID: "O1",
		Lines:           []OrderLine{{SKU: "A", Quantity: 100}, {SKU: "B", Quantity: 5}},
	})
	if !errors.Is(err, ErrInsufficientInventory) {
		t.Fatalf("want ErrInsufficientInventory, got %v", err)
	}

	// 订单不存在，且 A 的库存未被部分占用。
	if _, err := svc.GetFulfillmentDetail("O1"); !errors.Is(err, ErrOrderNotFound) {
		t.Fatalf("order should not exist: %v", err)
	}
	w1a := svc.GetInventory("W1", "A")
	if w1a.Available != 100 || w1a.Reserved != 0 {
		t.Errorf("W1/A should be untouched: %+v", w1a)
	}
}

func TestConfirmOrder_InvalidArguments(t *testing.T) {
	svc := newTestService(t)
	if _, err := svc.RegisterInventory(RegisterInventoryRequest{WarehouseID: "W1", SKU: "A", Quantity: 0}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("register qty 0: %v", err)
	}
	if _, err := svc.ConfirmOrder(ConfirmOrderRequest{ExternalOrderID: "", Lines: []OrderLine{{SKU: "A", Quantity: 1}}}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("empty order id: %v", err)
	}
	if _, err := svc.ConfirmOrder(ConfirmOrderRequest{ExternalOrderID: "O1", Lines: []OrderLine{{SKU: "A", Quantity: 0}}}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("zero line qty: %v", err)
	}
	if _, err := svc.ConfirmOrder(ConfirmOrderRequest{ExternalOrderID: "O1", Lines: nil}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("empty lines: %v", err)
	}
}

// ---------- 2. 幂等、冲突与并发防超卖 ----------

func TestConfirmOrder_IdempotentSameContent(t *testing.T) {
	svc := newTestService(t)
	mustRegister(t, svc, "W1", "A", 10)

	first := mustConfirm(t, svc, "O1", []OrderLine{{SKU: "A", Quantity: 4}})
	second, err := svc.ConfirmOrder(ConfirmOrderRequest{
		ExternalOrderID: "O1",
		Lines:           []OrderLine{{SKU: "A", Quantity: 4}}, // 同号同内容
	})
	if err != nil {
		t.Fatalf("idempotent reconfirm: %v", err)
	}
	if !second.Reconfirmed || second.Version != 1 || len(second.Allocations) != len(first.Allocations) {
		t.Fatalf("reconfirm mismatch: %+v", second)
	}
	if second.Allocations[0].ID != first.Allocations[0].ID {
		t.Fatalf("idempotent confirm should return original allocation")
	}
	// 库存只占用一次。
	inv := svc.GetInventory("W1", "A")
	if inv.Available != 6 || inv.Reserved != 4 {
		t.Errorf("inventory after reconfirm: %+v", inv)
	}
}

func TestConfirmOrder_ConflictOnContentChange(t *testing.T) {
	svc := newTestService(t)
	mustRegister(t, svc, "W1", "A", 100)
	mustRegister(t, svc, "W1", "B", 100)
	mustConfirm(t, svc, "O1", []OrderLine{{SKU: "A", Quantity: 4}})

	cases := [][]OrderLine{
		{{SKU: "A", Quantity: 5}},           // 数量变化
		{{SKU: "B", Quantity: 4}},           // 商品变化
		{{SKU: "A", Quantity: 4}, {"B", 1}}, // 增加商品
	}
	for i, lines := range cases {
		_, err := svc.ConfirmOrder(ConfirmOrderRequest{ExternalOrderID: "O1", Lines: lines})
		if !errors.Is(err, ErrOrderConflict) {
			t.Fatalf("case %d: want ErrOrderConflict, got %v", i, err)
		}
	}
	// 原方案不受影响。
	detail, _ := svc.GetFulfillmentDetail("O1")
	if detail.ActiveVersion != 1 || len(detail.Versions) != 1 {
		t.Fatalf("original plan mutated: %+v", detail.Versions)
	}
}

func TestConfirmOrder_ConcurrentNoOversell(t *testing.T) {
	svc := newTestService(t)
	mustRegister(t, svc, "W1", "A", 5)
	mustRegister(t, svc, "W2", "A", 5)

	// 30 个不同订单各抢 1 件，总共只有 10 件。
	const n = 30
	var wg sync.WaitGroup
	var mu sync.Mutex
	success, failInsufficient := 0, 0
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := svc.ConfirmOrder(ConfirmOrderRequest{
				ExternalOrderID: fmt.Sprintf("O%02d", i),
				Lines:           []OrderLine{{SKU: "A", Quantity: 1}},
			})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				success++
			case errors.Is(err, ErrInsufficientInventory):
				failInsufficient++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if success != 10 || failInsufficient != 20 {
		t.Fatalf("success=%d want 10, insufficient=%d want 20", success, failInsufficient)
	}
	for _, wh := range []string{"W1", "W2"} {
		inv := svc.GetInventory(wh, "A")
		if inv.Available != 0 || inv.Reserved != 5 {
			t.Errorf("%s/A oversold: %+v", wh, inv)
		}
	}
	assertInventoryInvariant(t, svc)
}

func TestConfirmOrder_ConcurrentSameOrderIdempotent(t *testing.T) {
	svc := newTestService(t)
	mustRegister(t, svc, "W1", "A", 100)

	const n = 20
	var wg sync.WaitGroup
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = svc.ConfirmOrder(ConfirmOrderRequest{
				ExternalOrderID: "DUP",
				Lines:           []OrderLine{{SKU: "A", Quantity: 3}},
			})
		}(i)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent confirm %d: %v", i, err)
		}
	}
	detail, _ := svc.GetFulfillmentDetail("DUP")
	if len(detail.Versions) != 1 {
		t.Fatalf("expected exactly 1 version, got %d", len(detail.Versions))
	}
	inv := svc.GetInventory("W1", "A")
	if inv.Reserved != 3 {
		t.Errorf("reserved = %d, want 3 (no duplicate hold)", inv.Reserved)
	}
}

// ---------- 3. 拣货回执、缺货重配、迟到回执 ----------

func setupShortageScenario(t *testing.T) (*FulfillmentService, *Allocation, *Allocation) {
	t.Helper()
	svc := newTestService(t)
	mustRegister(t, svc, "W1", "A", 7)
	mustRegister(t, svc, "W2", "A", 3)
	conf := mustConfirm(t, svc, "O1", []OrderLine{{SKU: "A", Quantity: 10}})
	w1 := allocByWh(conf.Allocations, "W1")
	w2 := allocByWh(conf.Allocations, "W2")
	if w1 == nil || w2 == nil || w1.Quantity != 7 || w2.Quantity != 3 {
		t.Fatalf("bad initial plan: %+v", conf.Allocations)
	}
	mustRegister(t, svc, "W3", "A", 5) // 后备库存：首版方案之后到货，短缺后由它承接缺口
	return svc, w1, w2
}

func TestPickingReceipt_FullPickedThenShip(t *testing.T) {
	svc, w1, w2 := setupShortageScenario(t)

	res, err := svc.PickingReceipt(PickingReceiptRequest{Items: []PickingReceiptItem{
		{AllocationID: w1.ID, PickedQty: 7},
		{AllocationID: w2.ID, PickedQty: 3},
	}})
	if err != nil {
		t.Fatalf("receipt: %v", err)
	}
	for _, v := range res.Updated {
		if v.Status != AllocPickedComplete {
			t.Errorf("alloc %s status = %s, want picked_complete", v.ID, v.Status)
		}
	}
	if res.ReallocatedVersion != 0 {
		t.Errorf("no shortage, should not reallocate, got version %d", res.ReallocatedVersion)
	}
	detail, _ := svc.GetFulfillmentDetail("O1")
	if detail.Status != StatusInFulfillment {
		t.Errorf("status = %s, want in_fulfillment", detail.Status)
	}

	ship1, err := svc.Ship(ShipmentRequest{AllocationID: w1.ID, Quantity: 7})
	if err != nil {
		t.Fatalf("ship W1: %v", err)
	}
	if ship1.Quantity != 7 || ship1.WarehouseID != "W1" {
		t.Fatalf("bad shipment: %+v", ship1)
	}
	if _, err := svc.Ship(ShipmentRequest{AllocationID: w2.ID, Quantity: 3}); err != nil {
		t.Fatalf("ship W2: %v", err)
	}
	detail, _ = svc.GetFulfillmentDetail("O1")
	if detail.Status != StatusShipped {
		t.Errorf("status = %s, want shipped", detail.Status)
	}
	assertNoDoubleShip(t, detail)
	assertInventoryInvariant(t, svc)
}

func TestPickingReceipt_ShortageTriggersReallocation(t *testing.T) {
	svc, w1, w2 := setupShortageScenario(t)

	// W2 拣货时全部短缺（3 件），W1 正常拣出 7。
	res, err := svc.PickingReceipt(PickingReceiptRequest{Items: []PickingReceiptItem{
		{AllocationID: w1.ID, PickedQty: 7},
		{AllocationID: w2.ID, PickedQty: 0, ShortageQty: 3},
	}})
	if err != nil {
		t.Fatalf("receipt: %v", err)
	}
	if res.ReallocateError != nil || res.ReallocatedVersion != 2 {
		t.Fatalf("reallocate: ver=%d err=%v", res.ReallocatedVersion, res.ReallocateError)
	}

	// v1 中 W2 的分配已关闭并核销；W1 拣出数量保留。
	detail, _ := svc.GetFulfillmentDetail("O1")
	if len(detail.Versions) != 2 {
		t.Fatalf("want 2 persisted versions, got %d", len(detail.Versions))
	}
	v1w2 := allocByWh(detail.Versions[0].Allocations, "W2")
	if v1w2.Status != AllocShortageClosed || v1w2.PickedQty != 0 || v1w2.ShortageQty != 3 {
		t.Errorf("v1 W2 alloc = %+v", v1w2)
	}
	v1w1 := allocByWh(detail.Versions[0].Allocations, "W1")
	if v1w1.PickedQty != 7 || v1w1.Status != AllocPickedComplete {
		t.Errorf("picked qty not preserved: %+v", v1w1)
	}

	// 缺口 3 件：先由可用库存最大的仓库承接。
	var gapTotal int
	for _, a := range res.Reallocated {
		gapTotal += a.Quantity
		if a.Version != 2 || a.Status != AllocActive {
			t.Errorf("v2 allocation bad: %+v", a)
		}
	}
	if gapTotal != 3 {
		t.Errorf("reallocated qty = %d, want 3", gapTotal)
	}

	// 台账：W1 onhand 7 = picked 7；W2 onhand 3 = lost 3；缺口 3 现被新仓 reserved。
	w1inv := svc.GetInventory("W1", "A")
	if w1inv.Picked != 7 || w1inv.Reserved != 0 {
		t.Errorf("W1 inv = %+v", w1inv)
	}
	w2inv := svc.GetInventory("W2", "A")
	if w2inv.Lost != 3 || w2inv.Available != 0 {
		t.Errorf("W2 inv = %+v", w2inv)
	}
	assertInventoryInvariant(t, svc)
}

func TestPickingReceipt_StaleReceiptRejectedAfterReallocation(t *testing.T) {
	svc, w1, w2 := setupShortageScenario(t)

	// W2 报短缺 -> 关闭并生成 v2。
	_, err := svc.PickingReceipt(PickingReceiptRequest{Items: []PickingReceiptItem{
		{AllocationID: w2.ID, ShortageQty: 3},
	}})
	if err != nil {
		t.Fatalf("receipt: %v", err)
	}

	// W2 的迟到“成功拣货”回执不得覆盖新分配，必须拒绝。
	_, err = svc.PickingReceipt(PickingReceiptRequest{Items: []PickingReceiptItem{
		{AllocationID: w2.ID, PickedQty: 3},
	}})
	if !errors.Is(err, ErrStaleAllocation) {
		t.Fatalf("stale receipt: want ErrStaleAllocation, got %v", err)
	}
	// 台账未被迟到回执污染。
	w2inv := svc.GetInventory("W2", "A")
	if w2inv.Picked != 0 || w2inv.Lost != 3 {
		t.Errorf("stale receipt mutated inventory: %+v", w2inv)
	}

	// 完整拣出后重复回执同样拒绝。
	_, err = svc.PickingReceipt(PickingReceiptRequest{Items: []PickingReceiptItem{
		{AllocationID: w1.ID, PickedQty: 7},
	}})
	if err != nil {
		t.Fatalf("first receipt: %v", err)
	}
	_, err = svc.PickingReceipt(PickingReceiptRequest{Items: []PickingReceiptItem{
		{AllocationID: w1.ID, PickedQty: 1},
	}})
	if !errors.Is(err, ErrStaleAllocation) {
		t.Fatalf("duplicate receipt: want ErrStaleAllocation, got %v", err)
	}
}

func TestPickingReceipt_PartialShortageSameAllocation(t *testing.T) {
	svc := newTestService(t)
	mustRegister(t, svc, "W1", "A", 7)
	mustRegister(t, svc, "W2", "A", 3)
	conf := mustConfirm(t, svc, "O1", []OrderLine{{SKU: "A", Quantity: 10}})
	w1 := allocByWh(conf.Allocations, "W1")
	mustRegister(t, svc, "W3", "A", 2) // 后备库存

	// W1 承担 7 件：拣出 5，短缺 2。
	res, err := svc.PickingReceipt(PickingReceiptRequest{Items: []PickingReceiptItem{
		{AllocationID: w1.ID, PickedQty: 5, ShortageQty: 2},
	}})
	if err != nil {
		t.Fatalf("receipt: %v", err)
	}
	if res.ReallocatedVersion != 2 {
		t.Fatalf("want v2, got %d (err=%v)", res.ReallocatedVersion, res.ReallocateError)
	}
	v1w1 := res.Updated[0]
	if v1w1.PickedQty != 5 || v1w1.ShortageQty != 2 || v1w1.Status != AllocShortageClosed {
		t.Errorf("v1 W1 = %+v", v1w1)
	}
	// 缺口 = 总需求 10 - 已拣 5 - W2 活跃 3 = 2。
	if total := res.Reallocated; len(total) == 0 || sumQty(total) != 2 {
		t.Errorf("want 2-unit gap plan, got %+v", total)
	}
	assertInventoryInvariant(t, svc)
}

func TestPickingReceipt_OverflowAndBatchAtomicity(t *testing.T) {
	svc, w1, w2 := setupShortageScenario(t)

	// 超量回执被拒绝。
	_, err := svc.PickingReceipt(PickingReceiptRequest{Items: []PickingReceiptItem{
		{AllocationID: w1.ID, PickedQty: 8},
	}})
	if !errors.Is(err, ErrReceiptOverflow) {
		t.Fatalf("want ErrReceiptOverflow, got %v", err)
	}

	// 批量原子性：一批中混入一条非法（已关闭/不存在）记录，整批不生效。
	_, err = svc.PickingReceipt(PickingReceiptRequest{Items: []PickingReceiptItem{
		{AllocationID: w1.ID, PickedQty: 7},
		{AllocationID: "does-not-exist", PickedQty: 1},
	}})
	if !errors.Is(err, ErrAllocationNotFound) {
		t.Fatalf("want ErrAllocationNotFound, got %v", err)
	}
	w1inv := svc.GetInventory("W1", "A")
	if w1inv.Reserved != 7 || w1inv.Picked != 0 {
		t.Errorf("batch should be atomic, but W1 mutated: %+v", w1inv)
	}

	// 同一批重复同一分配也拒绝。
	_, err = svc.PickingReceipt(PickingReceiptRequest{Items: []PickingReceiptItem{
		{AllocationID: w2.ID, PickedQty: 1},
		{AllocationID: w2.ID, PickedQty: 1},
	}})
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("dup in batch: want ErrInvalidArgument, got %v", err)
	}
}

func TestReallocate_ManualRetryAfterRestock(t *testing.T) {
	svc := newTestService(t)
	mustRegister(t, svc, "W1", "A", 5) // 全局仅 5 件
	conf := mustConfirm(t, svc, "O1", []OrderLine{{SKU: "A", Quantity: 5}})
	w1 := allocByWh(conf.Allocations, "W1")

	// W1 拣出 2，短缺 3，系统无其他库存，自动重配失败但拣出数量保留。
	res, err := svc.PickingReceipt(PickingReceiptRequest{Items: []PickingReceiptItem{
		{AllocationID: w1.ID, PickedQty: 2, ShortageQty: 3},
	}})
	if err != nil {
		t.Fatalf("receipt: %v", err)
	}
	if !errors.Is(res.ReallocateError, ErrInsufficientInventory) {
		t.Fatalf("want reallocate insufficient, got %v", res.ReallocateError)
	}
	detail, _ := svc.GetFulfillmentDetail("O1")
	if detail.LineSummary[0].Outstanding != 3 {
		t.Errorf("outstanding = %d, want 3", detail.LineSummary[0].Outstanding)
	}

	// 补货后手动重配成功。
	mustRegister(t, svc, "W3", "A", 3)
	re, err := svc.Reallocate(ReallocateRequest{OrderID: "O1"})
	if err != nil {
		t.Fatalf("manual reallocate: %v", err)
	}
	if re.Version != 2 || sumQty(re.Allocations) != 3 || re.Allocations[0].WarehouseID != "W3" {
		t.Fatalf("bad reallocation: %+v", re.Allocations)
	}

	// 没有缺口时再次重配返回 ErrNothingToReallocate。
	if _, err := svc.Reallocate(ReallocateRequest{OrderID: "O1"}); !errors.Is(err, ErrNothingToReallocate) {
		t.Fatalf("want ErrNothingToReallocate, got %v", err)
	}
	assertInventoryInvariant(t, svc)
}

// ---------- 4. 发货与取消 ----------

func TestShip_CannotExceedPicked(t *testing.T) {
	svc, w1, _ := setupShortageScenario(t)

	// 未拣货不能发货。
	if _, err := svc.Ship(ShipmentRequest{AllocationID: w1.ID, Quantity: 1}); !errors.Is(err, ErrShipExceedsPicked) {
		t.Fatalf("ship before pick: %v", err)
	}
	if _, err := svc.ReceiptPick(w1.ID, 7); err != nil {
		t.Fatalf("pick: %v", err)
	}
	// 拣 7 不能发 8。
	if _, err := svc.Ship(ShipmentRequest{AllocationID: w1.ID, Quantity: 8}); !errors.Is(err, ErrShipExceedsPicked) {
		t.Fatalf("overship: %v", err)
	}
	// 分批发 7 后再发被拒。
	if _, err := svc.Ship(ShipmentRequest{AllocationID: w1.ID, Quantity: 4}); err != nil {
		t.Fatalf("ship 4: %v", err)
	}
	if _, err := svc.Ship(ShipmentRequest{AllocationID: w1.ID, Quantity: 3}); err != nil {
		t.Fatalf("ship 3: %v", err)
	}
	if _, err := svc.Ship(ShipmentRequest{AllocationID: w1.ID, Quantity: 1}); !errors.Is(err, ErrShipExceedsPicked) {
		t.Fatalf("ship after exhausted: %v", err)
	}
}

func TestCancel_ReleasesOnlyUnpicked(t *testing.T) {
	svc, w1, w2 := setupShortageScenario(t)

	// W1 拣出 7，W2 尚未拣货。取消订单：只释放 W2 的 3 件预留。
	if _, err := svc.ReceiptPick(w1.ID, 7); err != nil {
		t.Fatalf("pick: %v", err)
	}
	detail, err := svc.Cancel(CancelRequest{OrderID: "O1"})
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if detail.Status != StatusCancelled {
		t.Errorf("status = %s, want cancelled", detail.Status)
	}
	w1inv := svc.GetInventory("W1", "A")
	w2inv := svc.GetInventory("W2", "A")
	if w1inv.Picked != 7 || w1inv.Available != 0 {
		t.Errorf("picked stock must be retained: W1 = %+v", w1inv)
	}
	if w2inv.Available != 3 || w2inv.Reserved != 0 {
		t.Errorf("unpicked reservation must be released: W2 = %+v", w2inv)
	}

	// 迟到拣货回执（针对被取消释放的分配）必须拒绝，防止重新占用已释放库存。
	if _, err := svc.ReceiptPick(w2.ID, 3); !errors.Is(err, ErrStaleAllocation) {
		t.Fatalf("pick after cancel: want ErrStaleAllocation, got %v", err)
	}
	// 取消后不能再重配。
	if _, err := svc.Reallocate(ReallocateRequest{OrderID: "O1"}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("reallocate after cancel: %v", err)
	}
	// 已拣出的 7 件仍可发货。
	if _, err := svc.Ship(ShipmentRequest{AllocationID: w1.ID, Quantity: 7}); err != nil {
		t.Fatalf("ship picked stock after cancel: %v", err)
	}
	assertInventoryInvariant(t, svc)
}

func TestCancel_Idempotent(t *testing.T) {
	svc, _, _ := setupShortageScenario(t)
	if _, err := svc.Cancel(CancelRequest{OrderID: "O1"}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	detail, err := svc.Cancel(CancelRequest{OrderID: "O1"})
	if err != nil {
		t.Fatalf("second cancel: %v", err)
	}
	if detail.Status != StatusCancelled {
		t.Errorf("status = %s", detail.Status)
	}
	inv := svc.GetInventory("W1", "A")
	if inv.Available != 7 {
		t.Errorf("double release? W1 available = %d, want 7", inv.Available)
	}
}

func TestCancelNotFound(t *testing.T) {
	svc := newTestService(t)
	if _, err := svc.Cancel(CancelRequest{OrderID: "X"}); !errors.Is(err, ErrOrderNotFound) {
		t.Fatalf("want ErrOrderNotFound, got %v", err)
	}
}

// ---------- 并发：取消 / 重配 / 发货 交错 ----------

func TestConcurrent_CancelReallocateShipConsistency(t *testing.T) {
	svc := newTestService(t)
	mustRegister(t, svc, "W1", "A", 48)
	mustRegister(t, svc, "W2", "A", 32) // 共 80 件，恰好满足 40 个订单各 2 件

	const orders = 40
	allocs := make([][]*Allocation, orders)
	for i := 0; i < orders; i++ {
		id := fmt.Sprintf("O%02d", i)
		conf := mustConfirm(t, svc, id, []OrderLine{{SKU: "A", Quantity: 2}})
		allocs[i] = conf.Allocations
	}

	// 每个订单随机交错执行：拣货、短缺回执、发货、取消、重配、查询。
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < orders; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			id := fmt.Sprintf("O%02d", i)
			for _, a := range allocs[i] {
				switch i % 4 {
				case 0:
					_, _ = svc.Ship(ShipmentRequest{AllocationID: a.ID, Quantity: 1}) // 未拣：预期失败
					_, _ = svc.ReceiptPick(a.ID, a.Quantity)
					_, _ = svc.Ship(ShipmentRequest{AllocationID: a.ID, Quantity: a.Quantity})
				case 1:
					_, _ = svc.Cancel(CancelRequest{OrderID: id})
					_, _ = svc.ReceiptPick(a.ID, a.Quantity) // 已取消：预期 ErrStale
				case 2:
					_, _ = svc.PickingReceipt(PickingReceiptRequest{Items: []PickingReceiptItem{
						{AllocationID: a.ID, ShortageQty: a.Quantity},
					}})
					_, _ = svc.Cancel(CancelRequest{OrderID: id})
				case 3:
					_, _ = svc.GetFulfillmentDetail(id)
					_, _ = svc.Reallocate(ReallocateRequest{OrderID: id})
				}
			}
			_, _ = svc.GetFulfillmentDetail(id)
		}()
	}
	close(start)
	wg.Wait()

	// 交错结束后做全量一致性审计。
	for i := 0; i < orders; i++ {
		id := fmt.Sprintf("O%02d", i)
		detail, err := svc.GetFulfillmentDetail(id)
		if err != nil {
			t.Fatalf("detail %s: %v", id, err)
		}
		assertNoDoubleShip(t, detail)

		// 每种 SKU：已发 + 在拣 + 已核销 + 预留 + 各仓可用 == 初始总量。
		var reserved, picked, lost int
		for _, v := range detail.Versions {
			for _, a := range v.Allocations {
				if a.Status == AllocActive {
					reserved += a.outstanding()
				}
				picked += a.PickedQty - a.ShippedQty
				lost += a.ShortageQty
			}
		}
		shipped := detail.LineSummary[0].Shipped
		outstanding := detail.LineSummary[0].Outstanding
		accounted := shipped + picked + lost + reserved
		if detail.Status == StatusCancelled {
			// 取消订单：未拣部分已释放回各仓可用（全局审计中校验），需求可不再全部挂账。
			if shipped+picked != 0 && shipped != picked {
				t.Errorf("cancelled %s: shipped=%d picked-unshipped=%d", id, shipped, picked)
			}
			_ = accounted
		} else {
			if accounted+outstanding != 2 {
				t.Errorf("order %s accounting: shipped=%d picked=%d lost=%d reserved=%d outstanding=%d",
					id, shipped, picked, lost, reserved, outstanding)
			}
		}
	}

	// 全局：两个仓 10 件货要么可用、要么在某订单的预留/拣货/发货/损耗中。
	globalAvail := svc.GetInventory("W1", "A").Available + svc.GetInventory("W2", "A").Available
	tied := 0
	for i := 0; i < orders; i++ {
		detail, _ := svc.GetFulfillmentDetail(fmt.Sprintf("O%02d", i))
		for _, l := range detail.LineSummary {
			tied += l.Picked - l.Shipped + l.Lost + l.Shipped
		}
		for _, v := range detail.Versions {
			for _, a := range v.Allocations {
				if a.Status == AllocActive {
					tied += a.outstanding()
				}
			}
		}
	}
	if globalAvail+tied != 80 {
		t.Errorf("global accounting: available=%d tied=%d sum=%d want 80", globalAvail, tied, globalAvail+tied)
	}
	assertInventoryInvariant(t, svc)
}

// ---------- 5. 履约明细 ----------

func TestGetFulfillmentDetail(t *testing.T) {
	svc := newTestService(t)
	mustRegister(t, svc, "W1", "A", 4)
	mustRegister(t, svc, "W2", "A", 6)
	conf := mustConfirm(t, svc, "O1", []OrderLine{{SKU: "A", Quantity: 10}})
	w1 := allocByWh(conf.Allocations, "W1")
	w2 := allocByWh(conf.Allocations, "W2")

	// W2 短缺 6 -> 无库存可重配 -> 补货 W3 -> 手动重配 -> 发货。
	res, err := svc.PickingReceipt(PickingReceiptRequest{Items: []PickingReceiptItem{
		{AllocationID: w1.ID, PickedQty: 4},
		{AllocationID: w2.ID, ShortageQty: 6},
	}})
	if err != nil || !errors.Is(res.ReallocateError, ErrInsufficientInventory) {
		t.Fatalf("receipt: err=%v reerr=%v", err, res.ReallocateError)
	}
	mustRegister(t, svc, "W3", "A", 6)
	re, err := svc.Reallocate(ReallocateRequest{OrderID: "O1"})
	if err != nil {
		t.Fatalf("reallocate: %v", err)
	}
	w3 := re.Allocations[0]
	if _, err := svc.ReceiptPick(w3.ID, 6); err != nil {
		t.Fatalf("pick W3: %v", err)
	}
	if _, err := svc.Ship(ShipmentRequest{AllocationID: w1.ID, Quantity: 4}); err != nil {
		t.Fatalf("ship W1: %v", err)
	}
	if _, err := svc.Ship(ShipmentRequest{AllocationID: w3.ID, Quantity: 6}); err != nil {
		t.Fatalf("ship W3: %v", err)
	}

	detail, err := svc.GetFulfillmentDetail("O1")
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	if len(detail.Versions) != 2 {
		t.Fatalf("versions persisted = %d, want 2", len(detail.Versions))
	}
	if detail.Versions[0].Note != "confirm" || detail.Versions[1].Note != "manual reallocate" {
		t.Errorf("version notes = %q, %q", detail.Versions[0].Note, detail.Versions[1].Note)
	}
	if len(detail.Shipments) != 2 {
		t.Errorf("shipments = %d, want 2", len(detail.Shipments))
	}
	if detail.Status != StatusShipped {
		t.Errorf("status = %s", detail.Status)
	}
	sum := detail.LineSummary[0]
	if sum.Demanded != 10 || sum.Shipped != 10 || sum.Lost != 6 || sum.Outstanding != 0 {
		t.Errorf("line summary = %+v", sum)
	}
	// 注意：Picked 是累计拣出（10），含已发部分。
	if sum.Picked != 10 {
		t.Errorf("cumulative picked = %d, want 10", sum.Picked)
	}
	assertNoDoubleShip(t, detail)
	assertInventoryInvariant(t, svc)
}

func TestGetFulfillmentDetail_NotFound(t *testing.T) {
	svc := newTestService(t)
	if _, err := svc.GetFulfillmentDetail("nope"); !errors.Is(err, ErrOrderNotFound) {
		t.Fatalf("want ErrOrderNotFound, got %v", err)
	}
}

// ---------- 辅助 ----------

func sumQty(allocs []*Allocation) int {
	n := 0
	for _, a := range allocs {
		n += a.Quantity
	}
	return n
}
