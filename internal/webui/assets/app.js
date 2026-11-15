(function () {
  "use strict";

  var page = document.body.getAttribute("data-page");
  function $(sel) { return document.querySelector(sel); }

  function shortId(id) { return id ? String(id).slice(0, 4) : "-"; }

  function clear(node) {
    while (node && node.firstChild) node.removeChild(node.firstChild);
  }

  function el(tag, text, cls) {
    var n = document.createElement(tag);
    if (text !== undefined && text !== null) n.textContent = String(text);
    if (cls) n.className = cls;
    return n;
  }

  function showError(msg) {
    var box = $("#error");
    if (!box) return;
    box.textContent = msg || "";
    box.hidden = !msg;
  }

  function api(path, opts) {
    var init = Object.assign({ cache: "no-store" }, opts || {});
    return fetch(path, init).then(function (res) {
      return res.json().catch(function () { return null; }).then(function (data) {
        if (!res.ok) {
          throw new Error((data && data.error) || ("HTTP " + res.status));
        }
        return data;
      });
    });
  }

  function poll(fn, ms) {
    var busy = false;
    function run() {
      if (document.hidden || busy) return;
      busy = true;
      Promise.resolve()
        .then(fn)
        .catch(function (e) { showError(e.message || String(e)); })
        .then(function () { busy = false; });
    }
    run();
    window.setInterval(run, ms || 2000);
    document.addEventListener("visibilitychange", function () {
      if (!document.hidden) run();
    });
  }

  function timeText(ms) {
    if (!ms) return "-";
    try { return new Date(ms).toLocaleString(); } catch (e) { return String(ms); }
  }

  function addCard(box, label, value) {
    var card = el("div", null, "card");
    card.appendChild(el("span", value, "value"));
    card.appendChild(el("span", label, "label"));
    box.appendChild(card);
  }

  function emptyRow(tbody, cols, text) {
    var tr = el("tr");
    var td = el("td", text || "（空）", "muted");
    td.colSpan = cols;
    tr.appendChild(td);
    tbody.appendChild(tr);
  }

  // ---------- index ----------

  function renderIndex() {
    return Promise.all([
      api("/api/node"),
      api("/api/status"),
      api("/api/peers"),
      api("/api/tickets"),
      api("/api/pages")
    ]).then(function (all) {
      var node = all[0], status = all[1], peers = all[2], tickets = all[3], pages = all[4];
      showError("");

      $("#node-line").textContent =
        (node.name || "-") + "#" + shortId(node.peer_id) +
        " · " + node.kind + " · " + node.network + " · " + node.version;

      var cards = $("#summary");
      clear(cards);
      var t = status.summary.tickets || {};
      addCard(cards, "运行", t.running || 0);
      addCard(cards, "完成", t.closed || 0);
      addCard(cards, "作废", t.voided || 0);
      addCard(cards, "待处理", (status.attention || []).length);
      addCard(cards, "outbox", (status.summary.outbox || {}).pending || 0);

      var att = $("#attention");
      clear(att);
      var items = status.attention || [];
      if (!items.length) {
        att.appendChild(el("li", "没有需要处理的事", "muted"));
      }
      items.forEach(function (a) {
        var li = el("li");
        li.appendChild(el("span", a.kind, "tag"));
        var text = "";
        if (a.ticket) text += " " + a.ticket.slice(0, 8) + "…";
        if (a.hop) text += " 第" + a.hop + "跳";
        if (a.peer) text += " → " + a.peer;
        if (a.reason) text += " · " + a.reason;
        if (a.age_sec) text += " · " + a.age_sec + " 秒";
        li.appendChild(document.createTextNode(text));
        if (a.hint) {
          li.appendChild(document.createElement("br"));
          li.appendChild(el("code", a.hint));
        }
        att.appendChild(li);
      });

      var cfg = $("#config");
      clear(cfg);
      function row(k, v) {
        cfg.appendChild(el("dt", k));
        cfg.appendChild(el("dd", v));
      }
      row("peer_id", node.peer_id);
      row("endpoint", node.endpoint);
      row("listen", node.listen);
      row("network", node.network);
      row("bootstrap", (node.bootstrap || []).join("  ") || "（无）");
      row("allow_from", (node.allow_from || []).join("  ") || "（无）");
      row("wake_mode", node.wake_mode || "（无）");

      var pt = $("#peers");
      clear(pt);
      var peerList = peers || [];
      if (!peerList.length) emptyRow(pt, 5, "名录为空");
      peerList.forEach(function (p) {
        var tr = el("tr");
        var name = (p.name || "-") + "#" + shortId(p.peer_id);
        if (p.deleted) name += "（已注销）";
        tr.appendChild(el("td", name, p.deleted ? "muted" : ""));
        tr.appendChild(el("td", p.kind + (p.wake_mode ? " / " + p.wake_mode : "")));
        tr.appendChild(el("td", p.endpoint, "mono"));
        tr.appendChild(el("td", p.version));
        tr.appendChild(el("td", p.desc));
        pt.appendChild(tr);
      });

      var tt = $("#tickets");
      clear(tt);
      var ticketList = tickets || [];
      if (!ticketList.length) emptyRow(tt, 4, "还没有工单");
      ticketList.forEach(function (s) {
        var tr = el("tr");
        var td = el("td");
        var a = el("a", s.id, "mono");
        a.href = "/ticket.html?id=" + encodeURIComponent(s.id);
        td.appendChild(a);
        tr.appendChild(td);
        tr.appendChild(el("td", s.status));
        var cur = "-";
        for (var i = 0; i < (s.hops || []).length; i++) {
          if (s.hops[i].state !== "verified") {
            cur = "第" + s.hops[i].step + "跳 " + shortId(s.hops[i].target) + " (" + s.hops[i].state + ")";
            break;
          }
        }
        tr.appendChild(el("td", cur));
        tr.appendChild(el("td", timeText(s.updated_at)));
        tt.appendChild(tr);
      });

      var pg = $("#pages");
      clear(pg);
      var list = pages || [];
      if (!list.length) pg.appendChild(el("li", "（还没有 Agent 发布的页面）", "muted"));
      list.forEach(function (p) {
        var li = el("li");
        var a = el("a", p.title);
        a.href = p.url;
        li.appendChild(a);
        if (p.desc) li.appendChild(el("span", " — " + p.desc, "muted"));
        pg.appendChild(li);
      });
    });
  }

  // ---------- new ----------

  function initNew() {
    var selected = [];

    function renderSelected() {
      var box = $("#selected");
      clear(box);
      if (!selected.length) {
        box.appendChild(el("li", "（尚未选择）", "muted"));
        return;
      }
      selected.forEach(function (name, i) {
        var li = el("li");
        li.appendChild(el("span", (i + 1) + ". " + name));
        var btn = el("button", "移除", "button small");
        btn.type = "button";
        btn.addEventListener("click", function () {
          selected.splice(i, 1);
          renderSelected();
        });
        li.appendChild(btn);
        box.appendChild(li);
      });
    }

    api("/api/peers").then(function (peers) {
      showError("");
      var box = $("#peer-list");
      clear(box);
      var list = (peers || []).filter(function (p) { return !p.deleted; });
      if (!list.length) {
        box.appendChild(el("span", "名录为空，无法发起工单", "muted"));
        return;
      }
      list.forEach(function (p) {
        var name = p.name || p.peer_id;
        var chip = el("button", name + "#" + shortId(p.peer_id) + " · " + p.kind, "chip");
        chip.type = "button";
        chip.title = p.desc || "";
        chip.addEventListener("click", function () {
          if (selected.indexOf(name) >= 0) return;
          selected.push(name);
          renderSelected();
        });
        box.appendChild(chip);
      });
    }).catch(function (e) { showError(e.message); });

    renderSelected();

    $("#submit").addEventListener("click", function () {
      showError("");
      if (!selected.length) {
        showError("至少选择一个目标节点");
        return;
      }
      var form;
      try {
        form = JSON.parse($("#form").value);
      } catch (e) {
        showError("表单不是合法 JSON：" + e.message);
        return;
      }
      api("/api/tickets", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ form: form, flow: selected })
      }).then(function (data) {
        window.location.href = "/ticket.html?id=" + encodeURIComponent(data.id);
      }).catch(function (e) { showError(e.message); });
    });
  }

  // ---------- ticket ----------

  function renderTicket() {
    var id = new URLSearchParams(window.location.search).get("id");
    if (!id) return Promise.reject(new Error("缺少工单号"));
    return api("/api/tickets/" + encodeURIComponent(id)).then(function (data) {
      showError("");
      var st = data.state;
      $("#ticket-title").textContent = "工单 " + st.id;
      $("#state").textContent = "状态 " + st.status + " · 创建 " + timeText(st.created_at) + " · 更新 " + timeText(st.updated_at);

      var hb = $("#hops");
      clear(hb);
      (st.hops || []).forEach(function (h) {
        var tr = el("tr");
        tr.appendChild(el("td", h.step));
        tr.appendChild(el("td", (h.target || "-") + (h.desc ? "（" + h.desc + "）" : ""), "mono"));
        tr.appendChild(el("td", h.state));
        tr.appendChild(el("td", timeText(h.accepted_at || h.sent_at)));
        hb.appendChild(tr);
      });

      $("#form").textContent = st.form ? JSON.stringify(st.form, null, 2) : "";

      var ab = $("#artifacts");
      clear(ab);
      var receipts = data.receipts || [];
      if (!receipts.length) ab.appendChild(el("li", "（还没有交付物）", "muted"));
      receipts.forEach(function (r) {
        var li = el("li");
        var a = el("a", "第" + r.hop + "跳 · " + r.size + " 字节 · 下载");
        a.href = "/api/blobs/" + encodeURIComponent(r.blob);
        li.appendChild(a);
        li.appendChild(el("span", "  " + r.blob, "mono muted small"));
        ab.appendChild(li);
      });

      var eb = $("#events");
      clear(eb);
      (data.events || []).forEach(function (ev) {
        var li = el("li");
        li.appendChild(el("span", "#" + ev.seq + " " + ev.type, "tag"));
        var line = " " + timeText(ev.ts);
        if (ev.hop) line += " · 第" + ev.hop + "跳";
        if (ev.peer) line += " · " + ev.peer;
        li.appendChild(document.createTextNode(line));
        if (ev.data) {
          li.appendChild(document.createElement("br"));
          li.appendChild(el("code", JSON.stringify(ev.data), "small"));
        }
        eb.appendChild(li);
      });
    });
  }

  if (page === "index") poll(renderIndex, 2000);
  if (page === "new") initNew();
  if (page === "ticket") poll(renderTicket, 2000);
})();
