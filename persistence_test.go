package goorderfulfillment

import (
	"path/filepath"
	"testing"
)

func TestSnapshot_SaveAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")

	svc, existed, err := NewServiceFromFile(path)
	if err != nil || existed {
		t.Fatalf("fresh load: existed=%v err=%v", existed, err)
	}
	must(t, svc.RegisterStock("WH-A", "SKU-X", 5))
	must(t, svc.RegisterStock("WH-B", "SKU-X", 5))

	plan, err := svc.ConfirmOrder("O-PERSIST", []OrderLine{{"SKU-X", 6}})
	must(t, err)
	// A 拣 4 件并声明短缺 1 件，缺口由 B 自动重配为 v2（B ATP=2，取 1）。
	_, err = svc.ReportPicking(PickingReceipt{
		ReceiptID: "r-a", OrderNumber: "O-PERSIST", PlanVersion: plan.Version,
		WarehouseID: "WH-A",
		Items:       []PickingLineItem{{SKU: "SKU-X", PickedQuantity: 4, Short: true}},
	})
	must(t, err)
	// B 拣货 + 部分发货。
	_, err = svc.ReportPicking(PickingReceipt{
		ReceiptID: "r-b1", OrderNumber: "O-PERSIST", PlanVersion: 1,
		WarehouseID: "WH-B", Items: []PickingLineItem{{SKU: "SKU-X", PickedQuantity: 1}},
	})
	must(t, err)
	_, err = svc.ReportPicking(PickingReceipt{
		ReceiptID: "r-b2", OrderNumber: "O-PERSIST", PlanVersion: 2,
		WarehouseID: "WH-B", Items: []PickingLineItem{{SKU: "SKU-X", PickedQuantity: 1}},
	})
	must(t, err)
	_, err = svc.ConfirmShipment(Shipment{
		ShipmentID: "sh-1", OrderNumber: "O-PERSIST", WarehouseID: "WH-B",
		Items: []ShipmentItem{{"SKU-X", 2}},
	})
	must(t, err)

	// 重启恢复。
	svc2, existed, err := NewServiceFromFile(path)
	must(t, err)
	if !existed {
		t.Fatal("snapshot should be detected on reload")
	}

	detail, err := svc2.GetFulfillment("O-PERSIST")
	must(t, err)
	if len(detail.Plans) != 2 {
		t.Fatalf("versions after reload = %d, want 2", len(detail.Plans))
	}
	if detail.Plans[0].Status != PlanSuperseded || detail.Plans[0].SupersededBy != 2 {
		t.Fatalf("v1 after reload wrong: %+v", detail.Plans[0])
	}
	if detail.Lines[0].Picked != 6 || detail.Lines[0].Shipped != 2 {
		t.Fatalf("progress after reload wrong: %+v", detail.Lines[0])
	}
	if detail.Order.Status != OrderPartiallyShipped {
		t.Fatalf("status after reload = %s", detail.Order.Status)
	}
	// 库存账面恢复：A 拣走 4 + 核销 1 -> OnHand 0；B 拣走 2 -> OnHand 3。
	if st := svc2.GetStock("WH-A", "SKU-X"); st.OnHand != 0 || st.Reserved != 0 {
		t.Fatalf("WH-A stock after reload: %+v", st)
	}
	if st := svc2.GetStock("WH-B", "SKU-X"); st.OnHand != 3 || st.Reserved != 0 {
		t.Fatalf("WH-B stock after reload: %+v", st)
	}

	// 已关闭分配的迟到回执仍被拒绝（使用全新回执 ID，以单独验证 staleness）。
	_, err = svc2.ReportPicking(PickingReceipt{
		ReceiptID: "r-a-late", OrderNumber: "O-PERSIST", PlanVersion: 1,
		WarehouseID: "WH-A", Items: []PickingLineItem{{SKU: "SKU-X", PickedQuantity: 1}},
	})
	assertErrIs(t, err, ErrStaleReceipt)
	// 幂等去重集合同样恢复：重复的旧回执 ID 返回 ErrReceiptDuplicate。
	_, err = svc2.ReportPicking(PickingReceipt{
		ReceiptID: "r-b2", OrderNumber: "O-PERSIST", PlanVersion: 2,
		WarehouseID: "WH-B", Items: []PickingLineItem{{SKU: "SKU-X", PickedQuantity: 1}},
	})
	assertErrIs(t, err, ErrReceiptDuplicate)
	_, err = svc2.ConfirmShipment(Shipment{
		ShipmentID: "sh-1", OrderNumber: "O-PERSIST", WarehouseID: "WH-B",
		Items: []ShipmentItem{{"SKU-X", 1}},
	})
	assertErrIs(t, err, ErrShipmentDuplicate)

	// 剩余拣货量发货后订单完成，并再次落盘。
	_, err = svc2.ConfirmShipment(Shipment{
		ShipmentID: "sh-2", OrderNumber: "O-PERSIST", WarehouseID: "WH-A",
		Items: []ShipmentItem{{"SKU-X", 4}},
	})
	must(t, err)
	o, _ := svc2.GetOrder("O-PERSIST")
	if o.Status != OrderShipped {
		t.Fatalf("final status = %s, want shipped", o.Status)
	}

	svc3, existed, err := NewServiceFromFile(path)
	must(t, err)
	if !existed {
		t.Fatal("snapshot missing")
	}
	o3, _ := svc3.GetOrder("O-PERSIST")
	if o3.Status != OrderShipped {
		t.Fatalf("status after second reload = %s", o3.Status)
	}
}
