/*
 * This Source Code Form is subject to the terms of the Mozilla Public
 *  License, v. 2.0. If a copy of the MPL was not distributed with this
 *  file, You can obtain one at https://mozilla.org/MPL/2.0/.
 */

import localFont from "next/font/local";
import "./globals.css";
import { WebSocketProvider } from './contexts/WebSocketContext';

const geistSans = localFont({
	src: "./fonts/GeistVF.woff",
	variable: "--font-geist-sans",
	weight: "100 900",
});
const geistMono = localFont({
	src: "./fonts/GeistMonoVF.woff",
	variable: "--font-geist-mono",
	weight: "100 900",
});

export const metadata = {
	title: "Create Next App",
	description: "Generated by create next app",
};

export default function RootLayout({ children }) {
	return (
		<html lang="en">
		<body
			className={`${geistSans.variable} ${geistMono.variable} antialiased`}
		>
		<WebSocketProvider>
			{children}
		</WebSocketProvider>
		</body>
		</html>
	);
}
