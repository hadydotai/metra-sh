// Mithril.js App
const App = {
    view: () => m(Dashboard)
}

const Dashboard = {
    oninit: (vnode) => {
        vnode.state.exchange = "binance"
        vnode.state.pair = "btcusdt"
        vnode.state.throttle = "2s"
        vnode.state.trades = []
        vnode.state.buffer = [] // Buffer for incoming trades when paused
        vnode.state.paused = false
        vnode.state.expandedId = null
        
        vnode.state.connected = false
        vnode.state.error = null
        vnode.state.stats = { high: 0, low: Infinity, vol: 0 }
        
        vnode.state.togglePause = () => {
            vnode.state.paused = !vnode.state.paused
            if (!vnode.state.paused) {
                // Resume: Flush buffer
                vnode.state.trades = [...vnode.state.buffer, ...vnode.state.trades].slice(0, 100)
                vnode.state.buffer = []
            }
        }

        vnode.state.setThrottle = (val) => {
            vnode.state.throttle = val
            vnode.state.connect()
        }

        vnode.state.toggleExpand = (id) => {
            if (vnode.state.expandedId === id) {
                vnode.state.expandedId = null
                // Optional: Auto-resume on collapse? Let's keep manual control for now or auto-resume if buffer empty
            } else {
                vnode.state.expandedId = id
                vnode.state.paused = true // Auto-pause on expand
            }
        }

        vnode.state.connect = async () => {
            if (vnode.state.es) {
                vnode.state.es.close()
            }
            vnode.state.trades = []
            vnode.state.buffer = []
            vnode.state.stats = { high: 0, low: Infinity, vol: 0 }
            vnode.state.error = null
            vnode.state.connected = false
            vnode.state.paused = false
            vnode.state.expandedId = null
            
            let url = `/stream/${vnode.state.exchange}/${vnode.state.pair}/trade`
            if (vnode.state.throttle && vnode.state.throttle !== "0ms") {
                url += `?throttle=${vnode.state.throttle}`
            }

            try {
                const res = await fetch(url, { method: 'GET' })
                if (!res.ok) {
                    const ct = res.headers.get("content-type")
                    if (ct && ct.includes("application/json")) {
                        const body = await res.json()
                        vnode.state.error = body.error || "Connection failed"
                    } else {
                        vnode.state.error = `Connection failed: ${res.statusText}`
                    }
                    m.redraw()
                    return
                }
            } catch (e) {
                vnode.state.error = e.message
                m.redraw()
                return
            }

            vnode.state.es = new EventSource(url)
            
            vnode.state.es.onopen = () => {
                vnode.state.connected = true
                vnode.state.error = null
                m.redraw()
            }
            
            vnode.state.es.onerror = () => {
                vnode.state.connected = false
                m.redraw()
            }

            vnode.state.es.onmessage = (e) => {
                const msg = JSON.parse(e.data)
                const trade = msg.payload
                
                // Parse numbers for stats
                const price = parseFloat(trade.price)
                const amount = parseFloat(trade.amount)

                // Update Stats (Always update stats even if paused)
                if (price > vnode.state.stats.high) vnode.state.stats.high = price
                if (price < vnode.state.stats.low) vnode.state.stats.low = price
                vnode.state.stats.vol += amount

                // List Logic
                if (vnode.state.paused) {
                    vnode.state.buffer.unshift(trade)
                } else {
                    vnode.state.trades.unshift(trade)
                    if (vnode.state.trades.length > 100) vnode.state.trades.pop()
                }
                
                m.redraw()
            }
        }

        // Initial connect
        vnode.state.connect()
    },

    onremove: (vnode) => {
        if (vnode.state.es) {
            vnode.state.es.close()
        }
    },

    view: (vnode) => {
        const s = vnode.state
        
        return m("div", {class: "space-y-6"}, [
            // Controls
            m("div", {class: "bg-gray-900 border border-gray-800 p-4 rounded-lg flex flex-col lg:flex-row gap-6 items-start lg:items-end justify-between"}, [
                m("div", {class: "flex flex-col md:flex-row gap-6 w-full lg:w-auto"}, [
                    // Exchange & Pair
                    m("div", {class: "flex gap-4"}, [
                        m("div", [
                            m("label", {class: "block text-xs text-gray-500 uppercase tracking-wider mb-1 font-bold"}, "Exchange"),
                            m("select", {
                                class: "bg-gray-950 border border-gray-700 text-white rounded px-3 py-2 text-sm focus:border-green-500 focus:outline-none h-10",
                                value: s.exchange,
                                onchange: (e) => { 
                                    s.exchange = e.target.value; 
                                    if (s.exchange === "bitstamp") s.pair = "btcusd";
                                    else if (s.exchange === "binance") s.pair = "btcusdt";
                                    s.connect() 
                                }
                            }, [
                                m("option", {value: "binance"}, "BINANCE"),
                                m("option", {value: "bitstamp"}, "BITSTAMP"),
                            ])
                        ]),
                        m("div", [
                            m("label", {class: "block text-xs text-gray-500 uppercase tracking-wider mb-1 font-bold"}, "Pair"),
                            m("input", {
                                class: "bg-gray-950 border border-gray-700 text-white rounded px-3 py-2 text-sm focus:border-green-500 focus:outline-none w-32 h-10 font-bold uppercase",
                                value: s.pair,
                                oninput: (e) => { s.pair = e.target.value },
                                onblur: () => s.connect(),
                                onkeydown: (e) => { if(e.key === "Enter") s.connect() },
                                placeholder: "BTCUSDT"
                            })
                        ]),
                    ]),

                    // Time Controls (Game-like)
                    m("div", [
                        m("label", {class: "block text-xs text-gray-500 uppercase tracking-wider mb-1 font-bold"}, "Update Rate"),
                        m("div", {class: "flex bg-gray-950 rounded p-1 border border-gray-800"}, [
                           ThrottleBtn(s, "0ms", "REALTIME"),
                           ThrottleBtn(s, "100ms", "100ms"),
                           ThrottleBtn(s, "500ms", "500ms"),
                           ThrottleBtn(s, "1s", "1s"),
                           ThrottleBtn(s, "2s", "2s"),
                        ])
                    ]),
                ]),

                // Status & Errors
                m("div", {class: "flex flex-col items-end gap-2"}, [
                    m("div", {class: "flex items-center gap-2"}, [
                        m("div", {class: "w-2 h-2 rounded-full " + (s.connected ? "bg-green-500 shadow-[0_0_8px_rgba(34,197,94,0.8)]" : "bg-red-500")}),
                        m("span", {class: "text-xs uppercase tracking-widest font-bold " + (s.connected ? "text-green-500" : "text-red-500")}, s.connected ? "LIVE FEED" : "DISCONNECTED")
                    ]),
                    s.error ? m("div", {class: "text-red-500 text-xs font-bold bg-red-900/20 px-2 py-1 rounded"}, "⚠️ " + s.error) : null
                ])
            ]),

            // Stats Cards
            m("div", {class: "grid grid-cols-2 md:grid-cols-4 gap-4"}, [
                StatCard("High", s.stats.high > 0 ? formatPrice(s.stats.high) : "--", "text-green-400"),
                StatCard("Low", s.stats.low < Infinity ? formatPrice(s.stats.low) : "--", "text-red-400"),
                StatCard("Volume", s.stats.vol.toFixed(4), "text-blue-400"),
                // Buffer/Pause Status
                m("div", {
                    class: "bg-gray-900 border border-gray-800 p-4 rounded-lg cursor-pointer hover:bg-gray-800 transition-colors " + (s.paused ? "border-yellow-500/50 bg-yellow-900/10" : ""),
                    onclick: s.togglePause
                }, [
                    m("div", {class: "text-xs text-gray-500 uppercase tracking-wider mb-1 font-bold"}, "Status"),
                    m("div", {class: "text-xl font-mono font-bold flex items-center gap-2 " + (s.paused ? "text-yellow-400" : "text-gray-400")}, [
                        m("span", s.paused ? "PAUSED" : "RUNNING"),
                        s.paused && s.buffer.length > 0 ? m("span", {class: "text-xs bg-yellow-500 text-black px-1.5 py-0.5 rounded-full"}, `+${s.buffer.length}`) : null
                    ])
                ])
            ]),

            // Table Container
            m("div", {class: "relative rounded-lg border border-gray-800 bg-gray-900/50 flex flex-col overflow-hidden"}, [
                // Header (Sticky)
                m("div", {class: "grid grid-cols-4 md:grid-cols-5 bg-gray-950 text-gray-400 uppercase text-xs font-bold tracking-wider border-b border-gray-800 px-4 py-3"}, [
                    m("div", "Time"),
                    m("div", "Side"),
                    m("div", {class: "text-right"}, "Price"),
                    m("div", {class: "text-right"}, "Amount"),
                    m("div", {class: "text-right hidden md:block"}, "ID"),
                ]),
                
                // Scrollable List
                m("div", {class: "overflow-y-auto custom-scrollbar", style: { maxHeight: "600px" }}, [
                     s.trades.length === 0 ? 
                        m("div", {class: "p-8 text-center text-gray-600 italic"}, "Waiting for trades...") :
                        s.trades.map(t => m(TradeRow, {
                            trade: t, 
                            expanded: s.expandedId === t.id,
                            onToggle: () => s.toggleExpand(t.id)
                        }))
                ])
            ])
        ])
    }
}

const ThrottleBtn = (s, val, label) => {
    const active = s.throttle === val
    return m("button", {
        class: "px-3 py-1.5 text-xs font-bold transition-all rounded-sm " + (active ? "bg-green-600 text-white shadow-lg" : "text-gray-400 hover:text-white hover:bg-gray-800"),
        onclick: () => s.setThrottle(val)
    }, label)
}

const StatCard = (label, value, colorClass) => {
    return m("div", {class: "bg-gray-900 border border-gray-800 p-4 rounded-lg"}, [
        m("div", {class: "text-xs text-gray-500 uppercase tracking-wider mb-1 font-bold"}, label),
        m("div", {class: "text-xl font-mono font-bold " + colorClass}, value)
    ])
}

const TradeRow = {
    view: (vnode) => {
        const t = vnode.attrs.trade
        const expanded = vnode.attrs.expanded
        const isSell = t.side === "sell"
        const sideColor = isSell ? "text-red-500" : "text-green-500"
        
        return m("div", [
            // Row
            m("div", {
                class: "grid grid-cols-4 md:grid-cols-5 px-4 py-2 hover:bg-gray-800/50 transition-colors font-mono text-sm cursor-pointer border-b border-gray-800/50 items-center " + (expanded ? "bg-gray-800/80 border-l-2 border-l-green-500" : "border-l-2 border-l-transparent"),
                onclick: vnode.attrs.onToggle
            }, [
                m("div", {class: "text-gray-400"}, formatTime(t.timestamp)),
                m("div", {class: "uppercase font-bold " + sideColor}, t.side),
                m("div", {class: "text-right text-white"}, formatPrice(t.price)),
                m("div", {class: "text-right text-gray-300"}, t.amount),
                m("div", {class: "text-right text-gray-600 text-xs hidden md:block"}, t.id.slice(-8)),
            ]),
            
            // Details Panel (Uncollapsed)
            expanded ? m("div", {class: "bg-gray-950/50 border-b border-gray-800 p-4 text-xs font-mono text-gray-400 grid grid-cols-1 md:grid-cols-2 gap-4 animate-[fadeIn_0.1s_ease-out]"}, [
                 m("div", [
                     m("div", {class: "uppercase tracking-widest text-xs text-gray-600 mb-1"}, "Full ID"),
                     m("div", {class: "text-white select-all"}, t.id)
                 ]),
                 m("div", [
                    m("div", {class: "uppercase tracking-widest text-xs text-gray-600 mb-1"}, "Raw Timestamp"),
                    m("div", {class: "text-white"}, t.timestamp)
                 ]),
                 m("div", {class: "col-span-1 md:col-span-2"}, [
                    m("div", {class: "uppercase tracking-widest text-xs text-gray-600 mb-1"}, "Normalized Payload"),
                    m("pre", {class: "bg-gray-950 p-2 rounded border border-gray-800 overflow-x-auto text-green-400"}, 
                        JSON.stringify(t, null, 2)
                    )
                 ])
            ]) : null
        ])
    }
}

function formatPrice(p) {
    if (typeof p === 'string') p = parseFloat(p)
    return new Intl.NumberFormat('en-US', { minimumFractionDigits: 2, maximumFractionDigits: 8 }).format(p)
}

function formatTime(ts) {
    return new Date(ts).toLocaleTimeString()
}

m.mount(document.getElementById("app"), App)
