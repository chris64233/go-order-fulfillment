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

// assertPackageConsistency 校验包裹与分配之间的一致性：
// 所有待出库包裹的商品总量 == 全部分配的 PackedQty 合计，
// 且已合并包裹永远保持 merged（旧回执不能恢复）。
func assertPackageConsistency(t *testing.T, svc *FulfillmentService) {
	t.Helper()
	pendingBySKU := map[string]int{}
	for _, p := range svc.packages {
		if p.Status != PackagePending {
			continue
		}
		for _, item := range p.Items {
			pendingBySKU[item.SKU] += item.Quantity
		}
	}
	packedBySKU := map[string]int{}
	for _, a := range svc.allocations {
		if a.PackedQty < 0 {
			t.Errorf("allocation %s negative packed qty %d", a.ID, a.PackedQty)
		}
		packedBySKU[a.LineSKU] += a.PackedQty
	}
	for sku, q := range packedBySKU {
		if pendingBySKU[sku] != q {
			t.Errorf("sku %s packed qty %d != pending packages total %d", sku, q, pendingBySKU[sku])
		}
	}
	for sku, q := range pendingBySKU {
		if packedBySKU[sku] != q {
			t.Errorf("sku %s pending packages total %d != packed qty %d", sku, q, packedBySKU[sku])
		}
	}
}

func mustPack(t *testing.T, svc *FulfillmentService, orderID string, items []PackItem) *PackageView {
	t.Helper()
	pkg, err := svc.PackItems(PackItemsRequest{
		OrderID:     orderID,
		Destination: "DEST-1",
		Carrier:     "CARRIER-A",
		Items:       items,
	})
	if err != nil {
		t.Fatalf("pack items for %s: %v", orderID, err)
	}
	return pkg
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

// ---------- 包裹合并 ----------

// setupMergeableOrder 构造一张两仓订单并全部拣齐，返回两条分配。
func setupMergeableOrder(t *testing.T, svc *FulfillmentService, orderID string) (alloc1, alloc2 *Allocation) {
	t.Helper()
	mustRegister(t, svc, "W1", "A", 5)
	mustRegister(t, svc, "W2", "A", 5)
	conf := mustConfirm(t, svc, orderID, []OrderLine{{SKU: "A", Quantity: 10}})
	alloc1 = allocByWh(conf.Allocations, "W1")
	alloc2 = allocByWh(conf.Allocations, "W2")
	if alloc1 == nil || alloc2 == nil {
		t.Fatalf("expected allocations in W1 and W2, got %+v", conf.Allocations)
	}
	if _, err := svc.ReceiptPick(alloc1.ID, alloc1.Quantity); err != nil {
		t.Fatalf("pick %s: %v", alloc1.ID, err)
	}
	if _, err := svc.ReceiptPick(alloc2.ID, alloc2.Quantity); err != nil {
		t.Fatalf("pick %s: %v", alloc2.ID, err)
	}
	return alloc1, alloc2
}

func TestMergePackages_HappyPath(t *testing.T) {
	svc := newTestService(t)
	alloc1, alloc2 := setupMergeableOrder(t, svc, "OM1")

	pkg1 := mustPack(t, svc, "OM1", []PackItem{{AllocationID: alloc1.ID, Quantity: 5}})
	pkg2 := mustPack(t, svc, "OM1", []PackItem{{AllocationID: alloc2.ID, Quantity: 5}})

	res, err := svc.MergePackages(MergePackagesRequest{
		MergeID:     "M1",
		OrderID:     "OM1",
		PackageIDs:  []string{pkg2.ID, pkg1.ID}, // 乱序传入，结果应确定
		Destination: "DEST-1",
		Carrier:     "CARRIER-A",
		Items:       []OrderLine{{SKU: "A", Quantity: 10}},
	})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if res.Replayed {
		t.Fatalf("first merge must not be a replay")
	}
	// 只产生一个新包裹。
	if res.NewPackage.ID == "" || res.NewPackage.ID == pkg1.ID || res.NewPackage.ID == pkg2.ID {
		t.Fatalf("unexpected new package %+v", res.NewPackage)
	}
	if res.NewPackage.Status != PackagePending {
		t.Errorf("new package status = %s, want pending", res.NewPackage.Status)
	}
	if len(res.NewPackage.Items) != 1 || res.NewPackage.Items[0].SKU != "A" || res.NewPackage.Items[0].Quantity != 10 {
		t.Errorf("new package items = %+v, want A:10", res.NewPackage.Items)
	}
	// 原包裹保留并标记为已合并，指向新包裹。
	if len(res.MergedPackages) != 2 {
		t.Fatalf("merged packages = %d, want 2", len(res.MergedPackages))
	}
	for _, mp := range res.MergedPackages {
		if mp.Status != PackageMerged {
			t.Errorf("source package %s status = %s, want merged", mp.ID, mp.Status)
		}
		if mp.MergedInto != res.NewPackage.ID {
			t.Errorf("source package %s MergedInto = %s, want %s", mp.ID, mp.MergedInto, res.NewPackage.ID)
		}
	}
	// 原包裹记录仍保留在服务内，未被删除。
	if svc.packages[pkg1.ID] == nil || svc.packages[pkg2.ID] == nil {
		t.Fatalf("merged source packages must be retained")
	}
	// 库存不变：合并不移动库存，商品仍在 Picked 桶。
	if inv := svc.GetInventory("W1", "A"); inv.Picked != 5 || inv.Shipped != 0 {
		t.Errorf("W1 inventory after merge = %+v, want picked=5 shipped=0", inv)
	}
	assertInventoryInvariant(t, svc)
	assertPackageConsistency(t, svc)
}

func TestMergePackages_RejectWhenAnyNotMergeable(t *testing.T) {
	svc := newTestService(t)
	alloc1, alloc2 := setupMergeableOrder(t, svc, "OM2")

	pkg1 := mustPack(t, svc, "OM2", []PackItem{{AllocationID: alloc1.ID, Quantity: 3}})
	pkg2 := mustPack(t, svc, "OM2", []PackItem{{AllocationID: alloc1.ID, Quantity: 2}})
	pkg3 := mustPack(t, svc, "OM2", []PackItem{{AllocationID: alloc2.ID, Quantity: 5}})
	// pkg3 已出库：不可再被合并。
	if _, err := svc.PackageOutbound(PackageOutboundRequest{PackageID: pkg3.ID}); err != nil {
		t.Fatalf("outbound pkg3: %v", err)
	}

	pkgCountBefore := len(svc.packages)
	_, err := svc.MergePackages(MergePackagesRequest{
		MergeID:     "M2",
		OrderID:     "OM2",
		PackageIDs:  []string{pkg1.ID, pkg2.ID, pkg3.ID},
		Destination: "DEST-1",
		Carrier:     "CARRIER-A",
		Items:       []OrderLine{{SKU: "A", Quantity: 10}},
	})
	if !errors.Is(err, ErrPackageNotMergeable) {
		t.Fatalf("want ErrPackageNotMergeable, got %v", err)
	}
	// 整单拒绝：不产生新包裹、不写合并记录，其余包裹保持 pending。
	if len(svc.packages) != pkgCountBefore {
		t.Errorf("package count changed after rejected merge: %d -> %d", pkgCountBefore, len(svc.packages))
	}
	if len(svc.merges) != 0 {
		t.Errorf("merge record created after rejected merge")
	}
	for _, id := range []string{pkg1.ID, pkg2.ID} {
		if svc.packages[id].Status != PackagePending {
			t.Errorf("package %s status = %s, want pending after rejected merge", id, svc.packages[id].Status)
		}
	}

	// 目的地不同同样整单拒绝。
	_, err = svc.MergePackages(MergePackagesRequest{
		MergeID:     "M2b",
		OrderID:     "OM2",
		PackageIDs:  []string{pkg1.ID, pkg2.ID},
		Destination: "DEST-OTHER",
		Carrier:     "CARRIER-A",
		Items:       []OrderLine{{SKU: "A", Quantity: 5}},
	})
	if !errors.Is(err, ErrPackageNotMergeable) {
		t.Fatalf("destination mismatch: want ErrPackageNotMergeable, got %v", err)
	}
	assertInventoryInvariant(t, svc)
	assertPackageConsistency(t, svc)
}

func TestMergePackages_IdempotentReplayAndConflicts(t *testing.T) {
	svc := newTestService(t)
	alloc1, alloc2 := setupMergeableOrder(t, svc, "OM3")
	pkg1 := mustPack(t, svc, "OM3", []PackItem{{AllocationID: alloc1.ID, Quantity: 5}})
	pkg2 := mustPack(t, svc, "OM3", []PackItem{{AllocationID: alloc2.ID, Quantity: 5}})

	base := MergePackagesRequest{
		MergeID:     "M3",
		OrderID:     "OM3",
		PackageIDs:  []string{pkg1.ID, pkg2.ID},
		Destination: "DEST-1",
		Carrier:     "CARRIER-A",
		Items:       []OrderLine{{SKU: "A", Quantity: 10}},
	}
	first, err := svc.MergePackages(base)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}

	// 相同合并号 + 相同内容重放：返回原结果，不产生新包裹。
	pkgCount := len(svc.packages)
	replay, err := svc.MergePackages(base)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !replay.Replayed {
		t.Errorf("replay must be marked as replayed")
	}
	if replay.NewPackage.ID != first.NewPackage.ID {
		t.Errorf("replay new package = %s, want original %s", replay.NewPackage.ID, first.NewPackage.ID)
	}
	if len(svc.packages) != pkgCount || len(svc.merges) != 1 {
		t.Errorf("replay created new state: packages=%d merges=%d", len(svc.packages), len(svc.merges))
	}

	// 包裹集合非法：单包裹、集合内重复。
	conflict := base
	conflict.PackageIDs = []string{pkg1.ID}
	if _, err := svc.MergePackages(conflict); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("single package: want ErrInvalidArgument, got %v", err)
	}
	conflict = base
	conflict.PackageIDs = []string{pkg1.ID, pkg2.ID, pkg1.ID}
	if _, err := svc.MergePackages(conflict); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("duplicate package: want ErrInvalidArgument, got %v", err)
	}

	// 目的地变化：冲突。
	conflict = base
	conflict.Destination = "DEST-2"
	if _, err := svc.MergePackages(conflict); !errors.Is(err, ErrMergeConflict) {
		t.Fatalf("destination change: want ErrMergeConflict, got %v", err)
	}

	// 商品明细变化：冲突。
	conflict = base
	conflict.Items = []OrderLine{{SKU: "A", Quantity: 9}}
	if _, err := svc.MergePackages(conflict); !errors.Is(err, ErrMergeConflict) {
		t.Fatalf("items change: want ErrMergeConflict, got %v", err)
	}

	// 仅承运约束变化：明确的阻断原因。
	conflict = base
	conflict.Carrier = "CARRIER-B"
	if _, err := svc.MergePackages(conflict); !errors.Is(err, ErrMergeCarrierConflict) {
		t.Fatalf("carrier change: want ErrMergeCarrierConflict, got %v", err)
	}
	assertInventoryInvariant(t, svc)
	assertPackageConsistency(t, svc)
}

func TestMergePackages_ItemMismatchAndCarrierRestriction(t *testing.T) {
	svc := newTestService(t)
	alloc1, alloc2 := setupMergeableOrder(t, svc, "OM4")
	pkg1 := mustPack(t, svc, "OM4", []PackItem{{AllocationID: alloc1.ID, Quantity: 3}})
	pkg2 := mustPack(t, svc, "OM4", []PackItem{{AllocationID: alloc2.ID, Quantity: 5}})

	// 声明明细与原包裹合计不符。
	_, err := svc.MergePackages(MergePackagesRequest{
		MergeID:     "M4",
		OrderID:     "OM4",
		PackageIDs:  []string{pkg1.ID, pkg2.ID},
		Destination: "DEST-1",
		Carrier:     "CARRIER-A",
		Items:       []OrderLine{{SKU: "A", Quantity: 10}},
	})
	if !errors.Is(err, ErrMergeItemMismatch) {
		t.Fatalf("want ErrMergeItemMismatch, got %v", err)
	}

	// 集合内包裹承运约束与申请不一致：明确阻断原因。
	pkg3, err := svc.PackItems(PackItemsRequest{
		OrderID: "OM4", Destination: "DEST-1", Carrier: "CARRIER-B",
		Items: []PackItem{{AllocationID: alloc1.ID, Quantity: 2}},
	})
	if err != nil {
		t.Fatalf("pack pkg3: %v", err)
	}
	_, err = svc.MergePackages(MergePackagesRequest{
		MergeID:     "M4b",
		OrderID:     "OM4",
		PackageIDs:  []string{pkg1.ID, pkg3.ID},
		Destination: "DEST-1",
		Carrier:     "CARRIER-A",
		Items:       []OrderLine{{SKU: "A", Quantity: 5}},
	})
	if !errors.Is(err, ErrCarrierRestriction) {
		t.Fatalf("want ErrCarrierRestriction, got %v", err)
	}
	assertInventoryInvariant(t, svc)
	assertPackageConsistency(t, svc)
}

func TestPackageOutbound_StaleReceiptCannotRestoreMerged(t *testing.T) {
	svc := newTestService(t)
	alloc1, alloc2 := setupMergeableOrder(t, svc, "OM5")
	pkg1 := mustPack(t, svc, "OM5", []PackItem{{AllocationID: alloc1.ID, Quantity: 5}})
	pkg2 := mustPack(t, svc, "OM5", []PackItem{{AllocationID: alloc2.ID, Quantity: 5}})

	res, err := svc.MergePackages(MergePackagesRequest{
		MergeID: "M5", OrderID: "OM5",
		PackageIDs:  []string{pkg1.ID, pkg2.ID},
		Destination: "DEST-1", Carrier: "CARRIER-A",
		Items: []OrderLine{{SKU: "A", Quantity: 10}},
	})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}

	// 旧包裹的迟到出库回执：拒绝，且不能恢复已合并记录。
	if _, err := svc.PackageOutbound(PackageOutboundRequest{PackageID: pkg1.ID}); !errors.Is(err, ErrStalePackage) {
		t.Fatalf("outbound merged pkg1: want ErrStalePackage, got %v", err)
	}
	if svc.packages[pkg1.ID].Status != PackageMerged {
		t.Fatalf("pkg1 status = %s, stale receipt restored merged package", svc.packages[pkg1.ID].Status)
	}
	if svc.packages[pkg2.ID].Status != PackageMerged {
		t.Fatalf("pkg2 status = %s, want merged", svc.packages[pkg2.ID].Status)
	}
	// 迟到回执没有扣减任何库存。
	if inv := svc.GetInventory("W1", "A"); inv.Picked != 5 || inv.Shipped != 0 {
		t.Fatalf("W1 inventory after stale receipt = %+v, want picked=5 shipped=0", inv)
	}

	// 新包裹正常出库：库存 Picked → Shipped，生成发货记录。
	out, err := svc.PackageOutbound(PackageOutboundRequest{PackageID: res.NewPackage.ID})
	if err != nil {
		t.Fatalf("outbound new package: %v", err)
	}
	if out.Status != PackageShipped {
		t.Errorf("new package status = %s, want shipped", out.Status)
	}
	if inv := svc.GetInventory("W1", "A"); inv.Picked != 0 || inv.Shipped != 5 {
		t.Errorf("W1 inventory after outbound = %+v, want picked=0 shipped=5", inv)
	}
	if inv := svc.GetInventory("W2", "A"); inv.Picked != 0 || inv.Shipped != 5 {
		t.Errorf("W2 inventory after outbound = %+v, want picked=0 shipped=5", inv)
	}
	detail, err := svc.GetFulfillmentDetail("OM5")
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	if len(detail.Shipments) != 2 {
		t.Errorf("shipments = %d, want 2 (one per source allocation)", len(detail.Shipments))
	}
	if detail.Status != StatusShipped {
		t.Errorf("order status = %s, want shipped", detail.Status)
	}

	// 重复出库回执：拒绝，不重复扣减库存。
	if _, err := svc.PackageOutbound(PackageOutboundRequest{PackageID: res.NewPackage.ID}); !errors.Is(err, ErrStalePackage) {
		t.Fatalf("duplicate outbound: want ErrStalePackage, got %v", err)
	}
	if inv := svc.GetInventory("W1", "A"); inv.Shipped != 5 {
		t.Errorf("W1 shipped = %d after duplicate receipt, want 5", inv.Shipped)
	}

	// 承运揽收：shipped → in_transit；承运中的包裹不可再合并。
	if _, err := svc.PackageTransit(PackageTransitRequest{PackageID: res.NewPackage.ID}); err != nil {
		t.Fatalf("transit: %v", err)
	}
	if svc.packages[res.NewPackage.ID].Status != PackageInTransit {
		t.Errorf("new package status = %s, want in_transit", svc.packages[res.NewPackage.ID].Status)
	}
	// 已合并的旧包裹也不能被揽收回执改变状态。
	if _, err := svc.PackageTransit(PackageTransitRequest{PackageID: pkg1.ID}); !errors.Is(err, ErrStalePackage) {
		t.Fatalf("transit merged pkg1: want ErrStalePackage, got %v", err)
	}
	if svc.packages[pkg1.ID].Status != PackageMerged {
		t.Errorf("pkg1 status = %s after stale transit receipt, want merged", svc.packages[pkg1.ID].Status)
	}
	assertInventoryInvariant(t, svc)
	assertPackageConsistency(t, svc)
}

func TestPackItems_ExceedsPickedAndBlocksDirectShip(t *testing.T) {
	svc := newTestService(t)
	mustRegister(t, svc, "W1", "A", 10)
	conf := mustConfirm(t, svc, "OM6", []OrderLine{{SKU: "A", Quantity: 10}})
	alloc := conf.Allocations[0]
	if _, err := svc.ReceiptPick(alloc.ID, 10); err != nil {
		t.Fatalf("pick: %v", err)
	}

	// 装箱数量超过已拣未装箱数量：拒绝。
	if _, err := svc.PackItems(PackItemsRequest{
		OrderID: "OM6", Destination: "DEST-1", Carrier: "CARRIER-A",
		Items: []PackItem{{AllocationID: alloc.ID, Quantity: 11}},
	}); !errors.Is(err, ErrPackExceedsPicked) {
		t.Fatalf("over-pack: want ErrPackExceedsPicked, got %v", err)
	}

	pkg := mustPack(t, svc, "OM6", []PackItem{{AllocationID: alloc.ID, Quantity: 6}})
	_ = pkg
	// 已装箱的 6 件不能再通过 Ship 直接发货，只能发未装箱的 4 件。
	if _, err := svc.Ship(ShipmentRequest{AllocationID: alloc.ID, Quantity: 5}); !errors.Is(err, ErrShipExceedsPicked) {
		t.Fatalf("ship packed qty: want ErrShipExceedsPicked, got %v", err)
	}
	if _, err := svc.Ship(ShipmentRequest{AllocationID: alloc.ID, Quantity: 4}); err != nil {
		t.Fatalf("ship unpacked qty: %v", err)
	}
	assertInventoryInvariant(t, svc)
	assertPackageConsistency(t, svc)
}

// 拣货、合并、取消并发：库存扣减与包裹状态必须保持一致。
func TestMergePackages_ConcurrentWithPickingAndCancel(t *testing.T) {
	svc := newTestService(t)
	mustRegister(t, svc, "W1", "A", 40)
	conf := mustConfirm(t, svc, "OM7", []OrderLine{{SKU: "A", Quantity: 40}})
	alloc := conf.Allocations[0]
	// 先拣出 20 件并装成两个包裹，剩余 20 件仍在预留中。
	if _, err := svc.ReceiptPick(alloc.ID, 20); err != nil {
		t.Fatalf("pick: %v", err)
	}
	pkg1 := mustPack(t, svc, "OM7", []PackItem{{AllocationID: alloc.ID, Quantity: 10}})
	pkg2 := mustPack(t, svc, "OM7", []PackItem{{AllocationID: alloc.ID, Quantity: 10}})

	var wg sync.WaitGroup
	// 取消：释放尚未拣货的 20 件预留。
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = svc.Cancel(CancelRequest{OrderID: "OM7"})
	}()
	// 合并：两个待出库包裹。
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = svc.MergePackages(MergePackagesRequest{
			MergeID: "M7", OrderID: "OM7",
			PackageIDs:  []string{pkg1.ID, pkg2.ID},
			Destination: "DEST-1", Carrier: "CARRIER-A",
			Items: []OrderLine{{SKU: "A", Quantity: 20}},
		})
	}()
	// 迟到拣货回执：取消后分配已关闭，必须被拒绝。
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = svc.ReceiptPick(alloc.ID, 20)
	}()
	// 旧包裹出库回执：与合并竞争，无论谁先谁后状态都必须一致。
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = svc.PackageOutbound(PackageOutboundRequest{PackageID: pkg1.ID})
	}()
	wg.Wait()

	assertInventoryInvariant(t, svc)
	assertPackageConsistency(t, svc)

	// 取消已生效：预留全部释放。
	if inv := svc.GetInventory("W1", "A"); inv.Reserved != 0 {
		t.Errorf("reserved = %d after cancel, want 0", inv.Reserved)
	}
	// 合并与旧包裹出库是互斥的：要么合并成功（两包裹 merged、新包裹 pending），
	// 要么旧包裹先出库（pkg1 shipped、合并因 pkg1 不可合并而失败）。
	merged := svc.merges["M7"] != nil
	if merged {
		if svc.packages[pkg1.ID].Status != PackageMerged || svc.packages[pkg2.ID].Status != PackageMerged {
			t.Errorf("merge succeeded but sources are %s/%s", svc.packages[pkg1.ID].Status, svc.packages[pkg2.ID].Status)
		}
		newPkg := svc.packages[svc.merges["M7"].NewPackageID]
		if newPkg.Status != PackagePending {
			t.Errorf("new package status = %s, want pending", newPkg.Status)
		}
	} else {
		if svc.packages[pkg1.ID].Status != PackageShipped {
			t.Errorf("merge rejected but pkg1 status = %s, want shipped", svc.packages[pkg1.ID].Status)
		}
		if svc.packages[pkg2.ID].Status != PackagePending {
			t.Errorf("merge rejected but pkg2 status = %s, want pending", svc.packages[pkg2.ID].Status)
		}
	}
	// 无论哪种交错：库存中的已拣+已发总量恒等于分配账上的已拣量
	// （迟到回执若先于取消到达则合法拣出 40，否则只有先前拣出的 20）。
	inv := svc.GetInventory("W1", "A")
	if got, want := inv.Picked+inv.Shipped, svc.allocations[alloc.ID].PickedQty; got != want {
		t.Errorf("picked+shipped = %d, want allocation picked qty %d", got, want)
	}
	// 分配要么被取消（取消先生效），要么已被迟到回执拣齐关闭（回执先生效）。
	if st := svc.allocations[alloc.ID].Status; st != AllocCancelled && st != AllocPickedComplete {
		t.Errorf("allocation status = %s, want cancelled or picked_complete", st)
	}
}

func TestGetFulfillmentDetail_PackageHistory(t *testing.T) {
	svc := newTestService(t)
	alloc1, alloc2 := setupMergeableOrder(t, svc, "OM8")
	pkg1 := mustPack(t, svc, "OM8", []PackItem{{AllocationID: alloc1.ID, Quantity: 5}})
	pkg2 := mustPack(t, svc, "OM8", []PackItem{{AllocationID: alloc2.ID, Quantity: 5}})
	res, err := svc.MergePackages(MergePackagesRequest{
		MergeID: "M8", OrderID: "OM8",
		PackageIDs:  []string{pkg1.ID, pkg2.ID},
		Destination: "DEST-1", Carrier: "CARRIER-A",
		Items: []OrderLine{{SKU: "A", Quantity: 10}},
	})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if _, err := svc.PackageOutbound(PackageOutboundRequest{PackageID: res.NewPackage.ID}); err != nil {
		t.Fatalf("outbound: %v", err)
	}

	detail, err := svc.GetFulfillmentDetail("OM8")
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	// 原包裹 + 新包裹全部展示，按 ID 升序（创建顺序），排序稳定。
	if len(detail.Packages) != 3 {
		t.Fatalf("packages = %d, want 3", len(detail.Packages))
	}
	wantIDs := []string{pkg1.ID, pkg2.ID, res.NewPackage.ID}
	wantStatus := []PackageStatus{PackageMerged, PackageMerged, PackageShipped}
	for i, p := range detail.Packages {
		if p.ID != wantIDs[i] {
			t.Errorf("packages[%d].ID = %s, want %s", i, p.ID, wantIDs[i])
		}
		if p.Status != wantStatus[i] {
			t.Errorf("packages[%d].Status = %s, want %s", i, p.Status, wantStatus[i])
		}
	}
	// 商品数量：新包裹合计 10 件。
	if got := detail.Packages[2].Items; len(got) != 1 || got[0].Quantity != 10 {
		t.Errorf("new package items = %+v, want A:10", got)
	}
	// 合并审计：原包裹集合、新包裹、承运约束齐全。
	if len(detail.Merges) != 1 {
		t.Fatalf("merges = %d, want 1", len(detail.Merges))
	}
	audit := detail.Merges[0]
	if audit.MergeID != "M8" || audit.NewPackageID != res.NewPackage.ID || audit.Carrier != "CARRIER-A" {
		t.Errorf("unexpected merge audit %+v", audit)
	}
	if len(audit.SourcePackageIDs) != 2 || audit.SourcePackageIDs[0] != pkg1.ID || audit.SourcePackageIDs[1] != pkg2.ID {
		t.Errorf("audit sources = %v, want [%s %s]", audit.SourcePackageIDs, pkg1.ID, pkg2.ID)
	}
	// 再次查询：结果完全一致（排序稳定、深拷贝互不影响）。
	again, err := svc.GetFulfillmentDetail("OM8")
	if err != nil {
		t.Fatalf("detail again: %v", err)
	}
	if fmt.Sprintf("%+v", detail.Packages) != fmt.Sprintf("%+v", again.Packages) ||
		fmt.Sprintf("%+v", detail.Merges) != fmt.Sprintf("%+v", again.Merges) {
		t.Errorf("repeated query returned different package history")
	}
	assertInventoryInvariant(t, svc)
	assertPackageConsistency(t, svc)
}
