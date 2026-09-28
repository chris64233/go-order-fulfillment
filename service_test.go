package goorderfulfillment

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

// 测试夹具：两个仓 WH-A / WH-B。
func newTestService(t *testing.T) *Service {
	t.Helper()
	svc := NewService()
	must(t, svc.RegisterStock("WH-A", "SKU-X", 5))
	must(t, svc.RegisterStock("WH-A", "SKU-Y", 2))
	must(t, svc.RegisterStock("WH-B", "SKU-X", 3))
	must(t, svc.RegisterStock("WH-B", "SKU-Z", 4))
	return svc
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func assertErrIs(t *testing.T, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("expected error %v, got %v", target, err)
	}
}

func allocQty(p *AllocationPlan, w WarehouseID, sku SKU) int {
	for _, a := range p.Allocations {
		if a.WarehouseID == w && a.SKU == sku {
			return a.Quantity
		}
	}
	return -1
}

// ---------- 1. 跨仓拆分与整体拒绝 ----------

func TestConfirmOrder_SplitAcrossWarehouses(t *testing.T) {
	svc := newTestService(t)

	// X 共需 7 件：A 有 5、B 有 3，方案必须跨仓且完整满足。
	plan, err := svc.ConfirmOrder("O1", []OrderLine{{"SKU-X", 7}, {"SKU-Y", 1}})
	must(t, err)
	if plan.Version != 1 || plan.Status != PlanActive {
		t.Fatalf("unexpected plan: v=%d status=%s", plan.Version, plan.Status)
	}
	if got := allocQty(plan, "WH-A", "SKU-X"); got != 5 {
		t.Fatalf("WH-A X alloc = %d, want 5", got)
	}
	if got := allocQty(plan, "WH-B", "SKU-X"); got != 2 {
		t.Fatalf("WH-B X alloc = %d, want 2", got)
	}

	// 占用后 ATP 正确：A 的 X 全部占用（ATP 0），B 的 X 剩 1。
	if st := svc.GetStock("WH-A", "SKU-X"); st.Reserved != 5 || st.Available() != 0 {
		t.Fatalf("WH-A X stock wrong: %+v", st)
	}
	if st := svc.GetStock("WH-B", "SKU-X"); st.Reserved != 2 || st.Available() != 1 {
		t.Fatalf("WH-B X stock wrong: %+v", st)
	}
}

func TestConfirmOrder_AllOrNothingRejection(t *testing.T) {
	svc := newTestService(t)

	// X 需要 8（总库存 8，可用）但同时 Z 需要 5（总库存仅 4）：
	// 必须整体拒绝且不留下任何占用。
	_, err := svc.ConfirmOrder("O-BAD", []OrderLine{{"SKU-X", 8}, {"SKU-Z", 5}})
	assertErrIs(t, err, ErrInsufficientStock)

	for _, v := range svc.ListStock() {
		if v.Reserved != 0 {
			t.Fatalf("stock should be untouched after rejection, got %+v", v)
		}
	}
	if _, err := svc.GetOrder("O-BAD"); !errors.Is(err, ErrOrderNotFound) {
		t.Fatalf("rejected order must not be created, got err=%v", err)
	}
}

func TestConfirmOrder_InvalidInput(t *testing.T) {
	svc := newTestService(t)
	for i, lines := range [][]OrderLine{
		nil,
		{{"SKU-X", 0}},
		{{"", 1}},
		{{"SKU-X", -1}},
	} {
		if _, err := svc.ConfirmOrder(OrderNumber(fmt.Sprintf("I%d", i)), lines); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("case %d: expected ErrInvalidInput, got %v", i, err)
		}
	}
}

// ---------- 2. 幂等、冲突与并发不超卖 ----------

func TestConfirmOrder_IdempotentAndConflict(t *testing.T) {
	svc := newTestService(t)

	plan1, err := svc.ConfirmOrder("O-IDEM", []OrderLine{{"SKU-X", 4}})
	must(t, err)
	// 相同内容（含重复行乱序）重复确认 -> 返回原方案。
	plan2, err := svc.ConfirmOrder("O-IDEM", []OrderLine{{"SKU-X", 2}, {"SKU-X", 2}})
	must(t, err)
	if plan2.Version != 1 || len(plan2.Allocations) != len(plan1.Allocations) {
		t.Fatalf("idempotent replay must return original plan v1, got %+v", plan2)
	}
	if svc.GetStock("WH-A", "SKU-X").Reserved != 4 {
		t.Fatalf("idempotent replay must not reserve stock twice")
	}

	// 内容变化（数量）-> 冲突。
	_, err = svc.ConfirmOrder("O-IDEM", []OrderLine{{"SKU-X", 5}})
	assertErrIs(t, err, ErrOrderConflict)
	// 内容变化（商品集合）-> 冲突。
	_, err = svc.ConfirmOrder("O-IDEM", []OrderLine{{"SKU-X", 4}, {"SKU-Y", 1}})
	assertErrIs(t, err, ErrOrderConflict)
	// 冲突不影响原方案与占用。
	if svc.GetStock("WH-A", "SKU-X").Reserved != 4 {
		t.Fatalf("conflict attempt must not change reservation")
	}
}

func TestConfirmOrder_ConcurrentNoOversell(t *testing.T) {
	// X 全渠道仅 8 件（A5 + B3）。50 个并发订单各抢 1 件，恰好 8 个成功。
	svc := newTestService(t)

	const n = 50
	var wg sync.WaitGroup
	var mu sync.Mutex
	confirmed, rejected := 0, 0
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			_, err := svc.ConfirmOrder(OrderNumber(fmt.Sprintf("C%02d", i)),
				[]OrderLine{{"SKU-X", 1}})
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				confirmed++
			} else if errors.Is(err, ErrInsufficientStock) {
				rejected++
			} else {
				t.Errorf("unexpected err: %v", err)
			}
		}()
	}
	wg.Wait()

	if confirmed != 8 || rejected != n-8 {
		t.Fatalf("confirmed=%d rejected=%d, want 8 / %d", confirmed, rejected, n-8)
	}
	// 库存恒等式校验：总预留 = 总售出件数，无负 ATP。
	reserved := 0
	for _, v := range svc.ListStock() {
		if v.Available < 0 || v.Reserved > v.OnHand {
			t.Fatalf("stock invariant broken: %+v", v)
		}
		reserved += v.Reserved
	}
	if reserved != 8 {
		t.Fatalf("total reserved = %d, want 8", reserved)
	}
}

func TestConfirmOrder_ConcurrentSameOrderIdempotent(t *testing.T) {
	svc := newTestService(t)
	const n = 20
	var wg sync.WaitGroup
	results := make([]error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			_, results[i] = svc.ConfirmOrder("O-SAME", []OrderLine{{"SKU-X", 2}})
		}()
	}
	wg.Wait()
	ok := 0
	for _, err := range results {
		if err == nil {
			ok++
		} else if !errors.Is(err, ErrOrderConflict) {
			t.Fatalf("unexpected err: %v", err)
		}
	}
	// 全部相同内容：所有调用都应成功（首个创建，其余幂等返回原方案）。
	if ok != n {
		t.Fatalf("idempotent concurrent confirms: %d/%d succeeded", ok, n)
	}
	if r := svc.GetStock("WH-A", "SKU-X").Reserved; r != 2 {
		t.Fatalf("reserved=%d, want 2", r)
	}
}

// ---------- 3. 拣货回执、部分短缺与缺货重配 ----------

func TestPicking_PartialShortageTriggersReplan(t *testing.T) {
	svc := newTestService(t)
	// X 7 件：A5 + B2；A 只拣到 3 件，缺口 4 应交给 B（B 可用 ATP=1）仍不够，
	// 所以重配失败，订单进入 awaiting_stock；补货后重配成功。
	plan, err := svc.ConfirmOrder("O-SHORT", []OrderLine{{"SKU-X", 7}})
	must(t, err)

	res, err := svc.ReportPicking(PickingReceipt{
		ReceiptID: "r1", OrderNumber: "O-SHORT", PlanVersion: plan.Version,
		WarehouseID: "WH-A",
		Items:       []PickingLineItem{{SKU: "SKU-X", PickedQuantity: 3, Short: true}},
	})
	must(t, err)
	if res.NewPlan != nil {
		t.Fatalf("replan should not succeed yet, got %+v", res.NewPlan)
	}
	if res.Status != OrderAwaitingStock {
		t.Fatalf("status=%s, want awaiting_stock", res.Status)
	}

	// v1 的 A 分配行：拣出 3 保留，缺口 2 关闭，占用已释放。
	v1, err := svc.GetPlan("O-SHORT", 1)
	must(t, err)
	a := v1.Allocations[0]
	if a.WarehouseID != "WH-A" || a.PickedQuantity != 3 || a.Status != AllocShortage {
		t.Fatalf("v1 WH-A alloc wrong: %+v", a)
	}
	// B 的 v1 分配 2 件仍在占用（尚未回执）。
	b := v1.Allocations[1]
	if b.Status != AllocPending || b.Quantity != 2 {
		t.Fatalf("v1 WH-B alloc should stay pending: %+v", b)
	}
	// A 的 OnHand 从 5 核销到 0（拣走 3 + 短缺核销 2），Reserved 清零。
	if st := svc.GetStock("WH-A", "SKU-X"); st.OnHand != 0 || st.Reserved != 0 {
		t.Fatalf("WH-A stock after shortage: %+v", st)
	}

	// B 拣满它负责的 2 件：此时已拣 5，缺口仍为 2，无在途 -> 仍是 awaiting_stock。
	res, err = svc.ReportPicking(PickingReceipt{
		OrderNumber: "O-SHORT", PlanVersion: 1, WarehouseID: "WH-B",
		Items: []PickingLineItem{{SKU: "SKU-X", PickedQuantity: 2}},
	})
	must(t, err)
	if res.Status != OrderAwaitingStock {
		t.Fatalf("status=%s, want awaiting_stock", res.Status)
	}

	// 补货 3 件给 B 后手动重配：缺口 2 全部落 B，生成 v2。
	must(t, svc.RegisterStock("WH-B", "SKU-X", 3))
	v2, err := svc.Reallocate("O-SHORT")
	must(t, err)
	if v2.Version != 2 || len(v2.Allocations) != 1 {
		t.Fatalf("unexpected v2: %+v", v2)
	}
	if got := allocQty(v2, "WH-B", "SKU-X"); got != 2 {
		t.Fatalf("v2 WH-B alloc = %d, want 2", got)
	}
	// v1 已被 v2 承接。
	v1, _ = svc.GetPlan("O-SHORT", 1)
	if v1.Status != PlanSuperseded || v1.SupersededBy != 2 {
		t.Fatalf("v1 should be superseded by 2: %+v", v1)
	}
}

func TestPicking_StaleReceiptRejected(t *testing.T) {
	svc := NewService()
	must(t, svc.RegisterStock("WH-A", "SKU-X", 3))
	must(t, svc.RegisterStock("WH-B", "SKU-X", 6))

	plan, err := svc.ConfirmOrder("O-STALE", []OrderLine{{"SKU-X", 6}})
	must(t, err)
	// A 明确报告整行短缺（拣 0、Short=true），缺口 3 立即由 B 重配成 v2。
	res, err := svc.ReportPicking(PickingReceipt{
		ReceiptID: "r1", OrderNumber: "O-STALE", PlanVersion: plan.Version,
		WarehouseID: "WH-A",
		Items:       []PickingLineItem{{SKU: "SKU-X", Short: true}},
	})
	must(t, err)
	if res.NewPlan == nil || res.NewPlan.Version != 2 {
		t.Fatalf("expected auto v2 replan, got %+v", res.NewPlan)
	}
	// A 旧分配已关闭：迟到的成功回执必须被拒绝，不能覆盖新分配。
	_, err = svc.ReportPicking(PickingReceipt{
		ReceiptID: "r-late", OrderNumber: "O-STALE", PlanVersion: 1,
		WarehouseID: "WH-A", Items: []PickingLineItem{{SKU: "SKU-X", PickedQuantity: 3}},
	})
	assertErrIs(t, err, ErrStaleReceipt)

	// 错误版本号找不到方案。
	_, err = svc.ReportPicking(PickingReceipt{
		OrderNumber: "O-STALE", PlanVersion: 9, WarehouseID: "WH-B",
		Items: []PickingLineItem{{SKU: "SKU-X", PickedQuantity: 3}},
	})
	assertErrIs(t, err, ErrPlanNotFound)

	// 超拣拒绝。
	_, err = svc.ReportPicking(PickingReceipt{
		OrderNumber: "O-STALE", PlanVersion: 1, WarehouseID: "WH-B",
		Items: []PickingLineItem{{SKU: "SKU-X", PickedQuantity: 99}},
	})
	assertErrIs(t, err, ErrOverPicked)

	// 重复 ReceiptID 拒绝（即使内容不同）。
	_, err = svc.ReportPicking(PickingReceipt{
		ReceiptID: "r1", OrderNumber: "O-STALE", PlanVersion: 2,
		WarehouseID: "WH-B", Items: []PickingLineItem{{SKU: "SKU-X", PickedQuantity: 1}},
	})
	assertErrIs(t, err, ErrReceiptDuplicate)
}

func TestPicking_ShortageDoesNotDoubleShip(t *testing.T) {
	// 核心不变量：短缺重配后，全流程发出总量绝不超过订单需求。
	svc := NewService()
	must(t, svc.RegisterStock("WH-A", "SKU-X", 4))
	must(t, svc.RegisterStock("WH-B", "SKU-X", 4))

	plan, err := svc.ConfirmOrder("O-DBL", []OrderLine{{"SKU-X", 4}})
	must(t, err)
	// A 明确报告 0 拣出短缺 -> 缺口 4 全部分给 B（v2）。
	_, err = svc.ReportPicking(PickingReceipt{
		ReceiptID: "a1", OrderNumber: "O-DBL", PlanVersion: plan.Version, WarehouseID: "WH-A",
		Items: []PickingLineItem{{SKU: "SKU-X", Short: true}},
	})
	must(t, err)
	// A 的迟到 4 件拣货回执不得生效。
	if _, err := svc.ReportPicking(PickingReceipt{
		ReceiptID: "a-late", OrderNumber: "O-DBL", PlanVersion: 1, WarehouseID: "WH-A",
		Items: []PickingLineItem{{SKU: "SKU-X", PickedQuantity: 4}},
	}); !errors.Is(err, ErrStaleReceipt) {
		t.Fatalf("late receipt must be stale, got %v", err)
	}
	// B 拣满并发货。
	mustPick := func() {
		_, err := svc.ReportPicking(PickingReceipt{
			ReceiptID: "b1", OrderNumber: "O-DBL", PlanVersion: 2, WarehouseID: "WH-B",
			Items: []PickingLineItem{{SKU: "SKU-X", PickedQuantity: 4}},
		})
		must(t, err)
	}
	mustPick()
	shipRes, err := svc.ConfirmShipment(Shipment{
		ShipmentID: "s1", OrderNumber: "O-DBL", WarehouseID: "WH-B",
		Items: []ShipmentItem{{"SKU-X", 4}},
	})
	must(t, err)
	if shipRes.Status != OrderShipped {
		t.Fatalf("status=%s, want shipped", shipRes.Status)
	}
	// A 没有任何拣货量，试图发货必须失败。
	_, err = svc.ConfirmShipment(Shipment{
		ShipmentID: "s2", OrderNumber: "O-DBL", WarehouseID: "WH-A",
		Items: []ShipmentItem{{"SKU-X", 1}},
	})
	assertErrIs(t, err, ErrOverShipped)

	detail, err := svc.GetFulfillment("O-DBL")
	must(t, err)
	totalShipped := 0
	for _, l := range detail.Lines {
		totalShipped += l.Shipped
		if l.Picked != 4 || l.Shipped != 4 {
			t.Fatalf("line progress wrong: %+v", l)
		}
	}
	if totalShipped != 4 {
		t.Fatalf("total shipped = %d, want 4", totalShipped)
	}
}

// ---------- 4. 发货与取消 ----------

func TestShipment_Validation(t *testing.T) {
	svc := newTestService(t)
	plan, err := svc.ConfirmOrder("O-SHIP", []OrderLine{{"SKU-X", 2}})
	must(t, err)
	// 未拣货不能发货。
	_, err = svc.ConfirmShipment(Shipment{
		OrderNumber: "O-SHIP", WarehouseID: "WH-A", Items: []ShipmentItem{{"SKU-X", 1}},
	})
	assertErrIs(t, err, ErrOverShipped)

	_, err = svc.ReportPicking(PickingReceipt{
		OrderNumber: "O-SHIP", PlanVersion: plan.Version, WarehouseID: "WH-A",
		Items: []PickingLineItem{{SKU: "SKU-X", PickedQuantity: 2}},
	})
	must(t, err)
	// 只能由真正拣货的仓发出。
	_, err = svc.ConfirmShipment(Shipment{
		ShipmentID: "s0", OrderNumber: "O-SHIP", WarehouseID: "WH-B",
		Items: []ShipmentItem{{"SKU-X", 1}},
	})
	assertErrIs(t, err, ErrOverShipped)

	// 分批发货：1 + 1，状态依次 picked_complete -> partially_shipped -> shipped。
	r1, err := svc.ConfirmShipment(Shipment{
		ShipmentID: "s1", OrderNumber: "O-SHIP", WarehouseID: "WH-A",
		Items: []ShipmentItem{{"SKU-X", 1}},
	})
	must(t, err)
	if r1.Status != OrderPartiallyShipped {
		t.Fatalf("status=%s, want partially_shipped", r1.Status)
	}
	// 重复发货 ID。
	_, err = svc.ConfirmShipment(Shipment{
		ShipmentID: "s1", OrderNumber: "O-SHIP", WarehouseID: "WH-A",
		Items: []ShipmentItem{{"SKU-X", 1}},
	})
	assertErrIs(t, err, ErrShipmentDuplicate)
	r2, err := svc.ConfirmShipment(Shipment{
		ShipmentID: "s2", OrderNumber: "O-SHIP", WarehouseID: "WH-A",
		Items: []ShipmentItem{{"SKU-X", 1}},
	})
	must(t, err)
	if r2.Status != OrderShipped {
		t.Fatalf("status=%s, want shipped", r2.Status)
	}
	// 已完成订单再发货 / 取消都拒绝。
	_, err = svc.ConfirmShipment(Shipment{
		OrderNumber: "O-SHIP", WarehouseID: "WH-A", Items: []ShipmentItem{{"SKU-X", 1}},
	})
	assertErrIs(t, err, ErrOverShipped)
	assertErrIs(t, svc.CancelOrder("O-SHIP"), ErrOrderTerminal)
}

func TestCancel_OnlyReleasesUnpicked(t *testing.T) {
	svc := newTestService(t)
	// X 5 全由 A 拣，Y 2 全由 A 拣。
	plan, err := svc.ConfirmOrder("O-CAN", []OrderLine{{"SKU-X", 5}, {"SKU-Y", 2}})
	must(t, err)
	// X 拣出 5（OnHand 减 5），Y 尚未拣。
	_, err = svc.ReportPicking(PickingReceipt{
		ReceiptID: "r1", OrderNumber: "O-CAN", PlanVersion: plan.Version, WarehouseID: "WH-A",
		Items: []PickingLineItem{{SKU: "SKU-X", PickedQuantity: 5}},
	})
	must(t, err)

	must(t, svc.CancelOrder("O-CAN"))
	// 取消幂等。
	must(t, svc.CancelOrder("O-CAN"))

	// Y 的 2 件占用被释放回 ATP；X 已拣走不回滚：OnHand=0,Reserved=0。
	if st := svc.GetStock("WH-A", "SKU-Y"); st.OnHand != 2 || st.Reserved != 0 || st.Available() != 2 {
		t.Fatalf("Y stock after cancel: %+v", st)
	}
	if st := svc.GetStock("WH-A", "SKU-X"); st.OnHand != 0 || st.Reserved != 0 {
		t.Fatalf("X stock after cancel: %+v", st)
	}
	o, _ := svc.GetOrder("O-CAN")
	if o.Status != OrderCancelled {
		t.Fatalf("status=%s", o.Status)
	}
	// 已拣未发的 X 仍不能发货（订单终态）。
	_, err = svc.ConfirmShipment(Shipment{
		OrderNumber: "O-CAN", WarehouseID: "WH-A", Items: []ShipmentItem{{"SKU-X", 1}},
	})
	assertErrIs(t, err, ErrOrderTerminal)
	// 取消后回执/重配也被拒。
	_, err = svc.Reallocate("O-CAN")
	assertErrIs(t, err, ErrOrderTerminal)
}

// ---------- 5. 取消 / 重配 / 发货并发一致性压测 ----------

func TestConcurrent_CancelReallocateShipConsistency(t *testing.T) {
	svc := NewService()
	must(t, svc.RegisterStock("WH-A", "SKU-X", 2))
	must(t, svc.RegisterStock("WH-B", "SKU-X", 10))

	plan, err := svc.ConfirmOrder("O-RACE", []OrderLine{{"SKU-X", 4}})
	must(t, err)

	var wg sync.WaitGroup
	// A 明确报告 0 拣出整行短缺（触发自动重配到 B）。
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = svc.ReportPicking(PickingReceipt{
			ReceiptID: "ra", OrderNumber: "O-RACE", PlanVersion: plan.Version, WarehouseID: "WH-A",
			Items: []PickingLineItem{{SKU: "SKU-X", Short: true}},
		})
	}()
	// 取消与重配抢跑。
	wg.Add(1)
	go func() { defer wg.Done(); _ = svc.CancelOrder("O-RACE") }()
	wg.Add(1)
	go func() { defer wg.Done(); _, _ = svc.Reallocate("O-RACE") }()
	wg.Wait()

	// 再补货、再试重配/发货，任何交错之后校验全局不变量。
	must(t, svc.RegisterStock("WH-B", "SKU-X", 5))
	_, _ = svc.Reallocate("O-RACE")
	_, _ = svc.ConfirmShipment(Shipment{
		ShipmentID: "post", OrderNumber: "O-RACE", WarehouseID: "WH-B",
		Items: []ShipmentItem{{"SKU-X", 4}},
	})

	o, _ := svc.GetOrder("O-RACE")
	detail, err := svc.GetFulfillment("O-RACE")
	must(t, err)

	// 库存恒等式：所有仓 Reserved 在 [0,OnHand]，无超卖。
	for _, v := range svc.ListStock() {
		if v.Reserved < 0 || v.Reserved > v.OnHand {
			t.Fatalf("stock invariant broken: %+v", v)
		}
	}
	// 发货恒等式：每 SKU 已发 <= 需求；已拣 == 已发（取消后未发即终态，
	// 未取消完成时二者也应相等）或已拣 >= 已发。
	for _, l := range detail.Lines {
		if l.Picked < l.Shipped || l.Shipped > l.Required {
			t.Fatalf("line invariant broken: %+v", l)
		}
	}
	// 取消与发货互斥：终态只能是其中一种走向。
	switch o.Status {
	case OrderCancelled, OrderShipped, OrderPartiallyShipped,
		OrderPickedComplete, OrderConfirmed, OrderAwaitingStock:
	default:
		t.Fatalf("illegal status: %s", o.Status)
	}
	if o.Status == OrderCancelled {
		// 取消赢了：不应有任何发货。
		for _, sh := range detail.Shipments {
			t.Fatalf("cancelled order must have no shipments, got %+v", sh)
		}
		// 全部未拣占用必须释放干净。
		for _, v := range svc.ListStock() {
			if v.Reserved != 0 {
				t.Fatalf("cancelled order leaked reservation: %+v", v)
			}
		}
	}
}

// 高并发混合操作：多个订单各自经历短缺、补拣、重配、取消/发货，
// 最终校验库存与履约恒等式。
func TestConcurrent_HighConcurrencyInvariants(t *testing.T) {
	svc := NewService()
	must(t, svc.RegisterStock("W1", "K", 100))
	must(t, svc.RegisterStock("W2", "K", 100))

	const orders = 40
	var wg sync.WaitGroup
	for i := 0; i < orders; i++ {
		i := i
		no := OrderNumber(fmt.Sprintf("O%02d", i))
		wg.Add(1)
		go func() {
			defer wg.Done()
			plan, err := svc.ConfirmOrder(no, []OrderLine{{"K", 5}})
			if err != nil {
				return
			}
			// 随机选一个仓回执（确定性地按编号选，避免 Math/rand 依赖）。
			w := WarehouseID("W1")
			if i%2 == 0 {
				w = "W2"
			}
			pick := 5
			short := false
			if i%3 == 0 {
				pick, short = 2, true // 部分短缺
			}
			_, _ = svc.ReportPicking(PickingReceipt{
				ReceiptID:   string(no) + "-r1",
				OrderNumber: no, PlanVersion: plan.Version, WarehouseID: w,
				Items: []PickingLineItem{{SKU: "K", PickedQuantity: pick, Short: short}},
			})
			if i%5 == 0 {
				_ = svc.CancelOrder(no)
				return
			}
			_, _ = svc.Reallocate(no)
			// 尝试把所有仓的拣货发掉（由服务校验数量，失败可接受）。
			for _, wh := range []WarehouseID{"W1", "W2"} {
				_, _ = svc.ConfirmShipment(Shipment{
					ShipmentID:  string(no) + "-" + string(wh),
					OrderNumber: no, WarehouseID: wh,
					Items: []ShipmentItem{{"K", 5}},
				})
			}
		}()
	}
	wg.Wait()

	// 全量校验。
	var totalReserved, totalPicked, totalShipped, totalRequired int
	for i := 0; i < orders; i++ {
		no := OrderNumber(fmt.Sprintf("O%02d", i))
		detail, err := svc.GetFulfillment(no)
		if err != nil {
			continue
		}
		for _, l := range detail.Lines {
			totalRequired += l.Required
			totalPicked += l.Picked
			totalShipped += l.Shipped
			if l.Picked < l.Shipped || l.Shipped > l.Required {
				t.Fatalf("%s invariant broken: %+v", no, l)
			}
		}
	}
	for _, v := range svc.ListStock() {
		if v.Reserved < 0 || v.Reserved > v.OnHand {
			t.Fatalf("stock invariant broken: %+v", v)
		}
		totalReserved += v.Reserved
	}
	if totalPicked < totalShipped {
		t.Fatalf("picked %d < shipped %d", totalPicked, totalShipped)
	}
	t.Logf("required=%d picked=%d shipped=%d reserved=%d",
		totalRequired, totalPicked, totalShipped, totalReserved)
}

// ---------- 履约明细查询 ----------

// 同一分配行允许先部分拣货（保持开放），后续补拣或再声明短缺。
func TestPicking_PartialThenShortOrComplete(t *testing.T) {
	svc := NewService()
	must(t, svc.RegisterStock("WH-A", "SKU-X", 3))
	must(t, svc.RegisterStock("WH-B", "SKU-X", 3))

	plan, err := svc.ConfirmOrder("O-P2", []OrderLine{{"SKU-X", 3}})
	must(t, err)
	// 第一次只拣 1 件，不声明短缺：分配行保持开放，订单仍是 confirmed。
	res, err := svc.ReportPicking(PickingReceipt{
		ReceiptID: "r1", OrderNumber: "O-P2", PlanVersion: plan.Version, WarehouseID: "WH-A",
		Items: []PickingLineItem{{SKU: "SKU-X", PickedQuantity: 1}},
	})
	must(t, err)
	if res.Status != OrderConfirmed {
		t.Fatalf("status=%s, want confirmed", res.Status)
	}
	v1, _ := svc.GetPlan("O-P2", 1)
	if v1.Allocations[0].Status != AllocPartial || v1.Allocations[0].PickedQuantity != 1 {
		t.Fatalf("alloc should be partial: %+v", v1.Allocations[0])
	}
	// 第二次补拣 2 件，分配行拣满，订单进入 picked_complete。
	res, err = svc.ReportPicking(PickingReceipt{
		ReceiptID: "r2", OrderNumber: "O-P2", PlanVersion: 1, WarehouseID: "WH-A",
		Items: []PickingLineItem{{SKU: "SKU-X", PickedQuantity: 2}},
	})
	must(t, err)
	if res.Status != OrderPickedComplete {
		t.Fatalf("status=%s, want picked_complete", res.Status)
	}
	// 再补拣必须报超拣。
	_, err = svc.ReportPicking(PickingReceipt{
		ReceiptID: "r3", OrderNumber: "O-P2", PlanVersion: 1, WarehouseID: "WH-A",
		Items: []PickingLineItem{{SKU: "SKU-X", PickedQuantity: 1}},
	})
	assertErrIs(t, err, ErrStaleReceipt) // 已关闭分配统一按 stale 拒绝
	if st := svc.GetStock("WH-A", "SKU-X"); st.OnHand != 0 || st.Reserved != 0 {
		t.Fatalf("stock wrong: %+v", st)
	}

	// 另一单：B 全量承接 3 件，部分拣货 1 件后声明短缺；此时 A 无库存，
	// 自动重配失败进入 awaiting_stock，补货后手动重配由 A 补齐 2 件。
	plan2, err := svc.ConfirmOrder("O-P3", []OrderLine{{"SKU-X", 3}})
	must(t, err)
	_, err = svc.ReportPicking(PickingReceipt{
		ReceiptID: "p3-1", OrderNumber: "O-P3", PlanVersion: plan2.Version, WarehouseID: "WH-B",
		Items: []PickingLineItem{{SKU: "SKU-X", PickedQuantity: 1}},
	})
	must(t, err)
	res, err = svc.ReportPicking(PickingReceipt{
		ReceiptID: "p3-2", OrderNumber: "O-P3", PlanVersion: 1, WarehouseID: "WH-B",
		Items: []PickingLineItem{{SKU: "SKU-X", Short: true}},
	})
	must(t, err)
	if res.NewPlan != nil || res.Status != OrderAwaitingStock {
		t.Fatalf("expect awaiting_stock without replan, got plan=%+v status=%s", res.NewPlan, res.Status)
	}
	must(t, svc.RegisterStock("WH-A", "SKU-X", 2))
	v2, err := svc.Reallocate("O-P3")
	must(t, err)
	if got := allocQty(v2, "WH-A", "SKU-X"); got != 2 {
		t.Fatalf("replan WH-A alloc = %d, want 2", got)
	}
	detail, _ := svc.GetFulfillment("O-P3")
	if detail.Lines[0].Picked != 1 {
		t.Fatalf("picked so far = %d, want 1", detail.Lines[0].Picked)
	}
}

func TestGetFulfillment_PersistsEveryVersion(t *testing.T) {
	svc := NewService()
	must(t, svc.RegisterStock("WH-A", "SKU-X", 2))
	must(t, svc.RegisterStock("WH-B", "SKU-X", 2))
	plan, err := svc.ConfirmOrder("O-HIST", []OrderLine{{"SKU-X", 4}})
	must(t, err)
	// A 拣 1 件并明确声明剩余 1 件短缺；B 把自己 v1 的 2 件拣满后，全局缺口 1。
	_, err = svc.ReportPicking(PickingReceipt{
		OrderNumber: "O-HIST", PlanVersion: plan.Version, WarehouseID: "WH-A",
		Items: []PickingLineItem{{SKU: "SKU-X", PickedQuantity: 1, Short: true}},
	})
	must(t, err)
	// B 把 v1 的 2 件拣满。
	_, err = svc.ReportPicking(PickingReceipt{
		OrderNumber: "O-HIST", PlanVersion: 1, WarehouseID: "WH-B",
		Items: []PickingLineItem{{SKU: "SKU-X", PickedQuantity: 2}},
	})
	must(t, err)
	// 补货后重配最后 1 件到 A（A 短缺核销后 OnHand=0，补货 1 后 ATP=1）。
	must(t, svc.RegisterStock("WH-A", "SKU-X", 1))
	v2, err := svc.Reallocate("O-HIST")
	must(t, err)
	_, err = svc.ReportPicking(PickingReceipt{
		ReceiptID: "r-last", OrderNumber: "O-HIST", PlanVersion: v2.Version,
		WarehouseID: "WH-A", Items: []PickingLineItem{{SKU: "SKU-X", PickedQuantity: 1}},
	})
	must(t, err)

	detail, err := svc.GetFulfillment("O-HIST")
	must(t, err)
	if len(detail.Plans) != 2 {
		t.Fatalf("expected 2 persisted plan versions, got %d", len(detail.Plans))
	}
	if detail.Plans[0].Status != PlanSuperseded || detail.Plans[1].Status != PlanCompleted {
		t.Fatalf("plan statuses wrong: %s / %s", detail.Plans[0].Status, detail.Plans[1].Status)
	}
	var picked int
	for _, l := range detail.Lines {
		picked += l.Picked
	}
	if picked != 4 {
		t.Fatalf("total picked = %d, want 4", picked)
	}
	if detail.Order.Status != OrderPickedComplete {
		t.Fatalf("status=%s, want picked_complete", detail.Order.Status)
	}
}
